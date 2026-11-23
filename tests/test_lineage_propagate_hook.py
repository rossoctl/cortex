"""The MCP context bridge of deploy/lineage-attach/lineage-propagate-hook.py.

Drives the MCP SDK's real streamable-HTTP transport: ``post_writer`` runs in
a background task started before the caller sets any context (the shape of a
session an app opens at startup and holds), the caller sends a request over
the session's memory stream, and an httpx mock transport records the OTel
context it sees at POST time. Needs ``mcp`` and ``opentelemetry-api``, which
the baked ``-otel`` image has and CI's Python job does not (skipped there), and
an mcp 1.x: on 2.x the SDK carries the context itself and the module skips.
In the image, from the repository root, with pytest and its dependencies
staged under /deps:

    podman run --rm -v "$PWD":/src:ro -v /path/to/pytest-deps:/deps:ro \\
      -e PYTHONPATH=/deps --entrypoint python <app>-otel:<tag> \\
      -m pytest /src/tests/test_lineage_propagate_hook.py -q
"""

import dataclasses
import importlib.util
import logging
import pathlib

import pytest

pytest.importorskip("mcp")
otel_context = pytest.importorskip("opentelemetry.context")
if importlib.util.find_spec("mcp.shared._context_streams"):
    pytest.skip("mcp >= 2.0 carries the context itself; the bridge stands down there", allow_module_level=True)

import anyio  # noqa: E402
import httpx  # noqa: E402
import mcp.client.streamable_http  # noqa: E402
import mcp.shared.message  # noqa: E402
from mcp import ClientSession, types  # noqa: E402
from mcp.client.streamable_http import StreamableHTTPTransport  # noqa: E402
from mcp.shared.message import SessionMessage  # noqa: E402
from mcp.types import JSONRPCMessage, JSONRPCNotification, JSONRPCRequest  # noqa: E402

HOOK = pathlib.Path(__file__).resolve().parents[1] / "deploy" / "lineage-attach" / "lineage-propagate-hook.py"
KEY = "lineage.test"


@pytest.fixture(autouse=True)
def unpatched_sdk(monkeypatch):
    """Every test starts from the stock SDK and leaves it that way."""
    monkeypatch.delenv("LINEAGE_PROPAGATE", raising=False)
    init, post = SessionMessage.__init__, StreamableHTTPTransport._handle_post_request
    yield
    SessionMessage.__init__ = init
    StreamableHTTPTransport._handle_post_request = post


def load_hook():
    spec = importlib.util.spec_from_file_location("lineage_propagate_hook", HOOK)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def json_result(request: httpx.Request) -> httpx.Response:
    """A JSON-RPC result for whatever request id the POST carried."""
    body = JSONRPCRequest.model_validate_json(request.read())
    return httpx.Response(200, json={"jsonrpc": "2.0", "id": body.id, "result": {}})


def start_session(tg, client):
    """A held session: the transport's writer task is started now, with no context, as at pod start."""
    read_w, read_r = anyio.create_memory_object_stream(8)
    write_w, write_r = anyio.create_memory_object_stream(8)
    transport = StreamableHTTPTransport("http://tool/mcp")
    tg.start_soon(transport.post_writer, client, write_r, read_w, write_w, lambda: None, tg)
    return write_w, read_r


def in_flight(n):
    """A wait that returns once n handlers are waiting on it."""
    arrived = 0
    every = anyio.Event()

    async def wait():
        nonlocal arrived
        arrived += 1
        if arrived == n:
            every.set()
        await every.wait()

    return wait


async def call_tools(values):
    """One held session per value; each sends a request while the caller's
    OTel context carries that value. Every POST is answered once all of them
    are in flight. Returns {value: context seen at POST}."""
    seen = {}
    all_in = in_flight(len(values))

    async def handler(request: httpx.Request) -> httpx.Response:
        tag = JSONRPCRequest.model_validate_json(request.read()).params["tag"]
        await all_in()
        seen[tag] = otel_context.get_value(KEY)
        return json_result(request)

    async with anyio.create_task_group() as tg, httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
        sessions = [(value, *start_session(tg, client)) for value in values]
        for value, write_w, _ in sessions:
            token = otel_context.attach(otel_context.set_value(KEY, value))
            try:
                request = JSONRPCRequest(jsonrpc="2.0", id=1, method="tools/call", params={"tag": value})
                await write_w.send(SessionMessage(message=JSONRPCMessage(request)))
            finally:
                otel_context.detach(token)
        for _, _, read_r in sessions:
            await read_r.receive()  # the response made it back through the transport
        tg.cancel_scope.cancel()
    return seen


def test_without_the_bridge_the_post_task_has_no_context():
    assert anyio.run(call_tools, ["a"]) == {"a": None}


def test_the_bridge_carries_the_callers_context_to_the_post():
    assert load_hook().bridge_mcp_context() is True
    assert anyio.run(call_tools, ["a"]) == {"a": "a"}


def test_concurrent_requests_on_different_sessions_keep_their_own_context():
    load_hook().bridge_mcp_context()
    assert anyio.run(call_tools, ["a", "b", "c"]) == {"a": "a", "b": "b", "c": "c"}


def test_a_request_sent_through_client_session_is_stamped_in_the_callers_task():
    """The SDK builds the SessionMessage inside ClientSession.send_request; the bridge
    relies on that happening in the caller's task, not in a sender task."""
    load_hook().bridge_mcp_context()
    seen = []

    async def handler(request: httpx.Request) -> httpx.Response:
        seen.append(otel_context.get_value(KEY))
        return json_result(request)

    async def scenario():
        async with anyio.create_task_group() as tg, httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
            write_w, read_r = start_session(tg, client)
            async with ClientSession(read_r, write_w) as session:
                ping = types.ClientRequest(types.PingRequest(method="ping"))
                token = otel_context.attach(otel_context.set_value(KEY, "turn"))
                try:
                    await session.send_request(ping, types.EmptyResult)
                finally:
                    otel_context.detach(token)
            tg.cancel_scope.cancel()

    anyio.run(scenario)
    assert seen == ["turn"]


def test_the_post_task_keeps_no_context_after_the_post():
    """A notification is posted inline in the writer task, so a context left attached there would
    leak into every later message of the session; the bridge detaches after each POST. The second
    message carries no stamp, so the POST sees whatever the writer task itself holds."""
    load_hook().bridge_mcp_context()
    seen = []
    both = anyio.Event()

    async def handler(request: httpx.Request) -> httpx.Response:
        seen.append(otel_context.get_value(KEY))
        if len(seen) == 2:
            both.set()
        return httpx.Response(202)

    async def scenario():
        async with anyio.create_task_group() as tg, httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
            write_w, _ = start_session(tg, client)
            notification = JSONRPCMessage(JSONRPCNotification(jsonrpc="2.0", method="n"))
            token = otel_context.attach(otel_context.set_value(KEY, "first"))
            try:
                await write_w.send(SessionMessage(message=notification))
            finally:
                otel_context.detach(token)
            unstamped = SessionMessage(message=notification)
            del unstamped._lineage_otel_context
            await write_w.send(unstamped)
            await both.wait()
            tg.cancel_scope.cancel()

    anyio.run(scenario)
    assert seen == ["first", None]


def test_installing_twice_patches_once():
    hook = load_hook()
    hook.bridge_mcp_context()
    init, post = SessionMessage.__init__, StreamableHTTPTransport._handle_post_request
    assert hook.bridge_mcp_context() is True
    assert (SessionMessage.__init__, StreamableHTTPTransport._handle_post_request) == (init, post)


def test_an_empty_stamp_does_not_replace_a_post_task_that_has_context():
    """A message built with no context (a startup worker draining a queue) posted by a writer that
    was started inside a span: the writer's own context must survive, as on the stock SDK."""
    load_hook().bridge_mcp_context()
    seen = []

    async def handler(request: httpx.Request) -> httpx.Response:
        seen.append(otel_context.get_value(KEY))
        return json_result(request)

    async def scenario():
        async with anyio.create_task_group() as tg, httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
            token = otel_context.attach(otel_context.set_value(KEY, "writer-had-this"))
            try:
                write_w, read_r = start_session(tg, client)  # the writer task inherits this context
            finally:
                otel_context.detach(token)
            request = JSONRPCRequest(jsonrpc="2.0", id=1, method="tools/call", params={})
            await write_w.send(SessionMessage(message=JSONRPCMessage(request)))  # stamped with the empty context
            await read_r.receive()
            tg.cancel_scope.cancel()

    anyio.run(scenario)
    assert seen == ["writer-had-this"]


def test_two_requests_in_flight_on_one_session_keep_their_own_context():
    load_hook().bridge_mcp_context()
    seen = {}

    async def scenario():
        all_in = in_flight(2)  # both POSTs are answered once both are in flight

        async def handler(request: httpx.Request) -> httpx.Response:
            tag = JSONRPCRequest.model_validate_json(request.read()).params["tag"]
            await all_in()
            seen[tag] = otel_context.get_value(KEY)
            return json_result(request)

        async with anyio.create_task_group() as tg, httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
            write_w, read_r = start_session(tg, client)
            for i, value in enumerate(("x", "y"), start=1):
                token = otel_context.attach(otel_context.set_value(KEY, value))
                try:
                    request = JSONRPCRequest(jsonrpc="2.0", id=i, method="tools/call", params={"tag": value})
                    await write_w.send(SessionMessage(message=JSONRPCMessage(request)))
                finally:
                    otel_context.detach(token)
            await read_r.receive()
            await read_r.receive()
            tg.cancel_scope.cancel()

    anyio.run(scenario)
    assert seen == {"x": "x", "y": "y"}


def test_the_wrapper_passes_positional_and_keyword_calls_through(monkeypatch):
    """Both call shapes the SDK could use reach the original; an object without a stamp is passed as is."""
    calls = []

    async def original(self, ctx):
        calls.append(ctx)

    monkeypatch.setattr(StreamableHTTPTransport, "_handle_post_request", original)
    load_hook().bridge_mcp_context()
    transport = StreamableHTTPTransport("http://tool/mcp")
    anyio.run(StreamableHTTPTransport._handle_post_request, transport, object())
    anyio.run(lambda: StreamableHTTPTransport._handle_post_request(transport, ctx="kw"))
    assert len(calls) == 2 and calls[1] == "kw"


@pytest.mark.parametrize(
    "break_it",
    [
        lambda mp: mp.delattr(StreamableHTTPTransport, "_handle_post_request"),
        lambda mp: mp.setattr(StreamableHTTPTransport, "_handle_post_request", lambda self, ctx, more: None),
        lambda mp: mp.setattr(
            mcp.client.streamable_http, "RequestContext", dataclasses.make_dataclass("Ctx", ["client", "message"])
        ),
        lambda mp: mp.setattr(
            mcp.shared.message, "SessionMessage", dataclasses.make_dataclass("Closed", ["message"], slots=True)
        ),
    ],
    ids=["no method", "other signature", "no session_message", "slotted message"],
)
def test_an_sdk_without_the_seams_is_left_untouched_with_one_warning(monkeypatch, caplog, break_it):
    break_it(monkeypatch)
    init, post = SessionMessage.__init__, getattr(StreamableHTTPTransport, "_handle_post_request", None)
    with caplog.at_level(logging.WARNING):
        assert load_hook().bridge_mcp_context() is False
    assert SessionMessage.__init__ is init
    assert getattr(StreamableHTTPTransport, "_handle_post_request", None) is post
    assert [r.levelname for r in caplog.records] == ["WARNING"]
    assert "MCP context bridge not installed" in caplog.text


def test_an_sdk_that_carries_the_context_itself_is_left_alone(monkeypatch, caplog):
    """mcp 2.x has mcp.shared._context_streams; the bridge stands down without a word."""
    monkeypatch.setitem(__import__("sys").modules, "mcp.shared._context_streams", mcp.shared.message)
    init, post = SessionMessage.__init__, StreamableHTTPTransport._handle_post_request
    with caplog.at_level(logging.WARNING):
        assert load_hook().bridge_mcp_context() is False
    assert (SessionMessage.__init__, StreamableHTTPTransport._handle_post_request) == (init, post)
    assert caplog.records == []


def test_an_installed_mcp_that_fails_to_import_is_not_swallowed(monkeypatch):
    """A broken transitive dependency (the module exists, the name does not): the hook's outer guard
    must get to log it, so the bridge does not stay off in silence."""
    empty = type(mcp)("mcp.client.streamable_http")  # a module with none of the names the hook imports
    monkeypatch.setitem(__import__("sys").modules, "mcp.client.streamable_http", empty)
    with pytest.raises(ImportError):
        load_hook().bridge_mcp_context()


def test_without_mcp_the_bridge_is_silent(monkeypatch, caplog):
    monkeypatch.setitem(__import__("sys").modules, "mcp.client.streamable_http", None)
    with caplog.at_level(logging.WARNING):
        assert load_hook().bridge_mcp_context() is False
    assert caplog.records == []
