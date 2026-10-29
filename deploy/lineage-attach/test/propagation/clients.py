"""One outbound call per client library, to TARGET, printing what the sink
echoed. With INBOUND_TRACEPARENT set the call runs inside a span continued
from it, so `carried` can check the trace id survives. Usage: clients.py <row>
Imports stay inside each function: a probe image carries one library."""

import os
import sys


def httpx_(url):
    import httpx

    return httpx.get(url, timeout=5).text


def requests_(url):
    import requests

    return requests.get(url, timeout=5).text


def aiohttp_client(url):
    import asyncio

    import aiohttp

    async def get():
        async with aiohttp.ClientSession() as s:
            async with s.get(url) as r:
                return await r.text()

    return asyncio.run(get())


def urllib3_(url):
    import urllib3

    return urllib3.PoolManager().request("GET", url, timeout=5).data.decode()


def urllib_(url):
    import urllib.request

    return urllib.request.urlopen(url, timeout=5).read().decode()


def threading_(url):
    """From a worker thread: without -threading the context stays on the
    caller's thread and the hop leaves unstamped."""
    from concurrent.futures import ThreadPoolExecutor

    return ThreadPoolExecutor(1).submit(requests_, url).result()


def grpc_(target):
    """Client half of the gRPC row; the server half is servers.py grpc."""
    import grpc

    ident = lambda b: b  # noqa: E731  raw bytes, no proto
    channel = grpc.insecure_channel(target)
    grpc.channel_ready_future(channel).result(timeout=30)  # the server may still be starting
    echo = channel.unary_unary("/probe.Echo/Echo", request_serializer=ident, response_deserializer=ident)
    return echo(b"", timeout=5).decode()


CLIENTS = {
    "httpx": httpx_,
    "requests": requests_,
    "aiohttp_client": aiohttp_client,
    "urllib3": urllib3_,
    "urllib": urllib_,
    "threading": threading_,
    "floor": requests_,  # requests on an app that already carries opentelemetry-api 1.34.0
    "grpc": grpc_,
}

if __name__ == "__main__":
    call, target = CLIENTS[sys.argv[1]], os.environ["TARGET"]
    inbound = os.environ.get("INBOUND_TRACEPARENT")
    if not inbound:
        print(call(target))
    else:
        from opentelemetry import trace
        from opentelemetry.propagate import extract

        ctx = extract({"traceparent": inbound})
        with trace.get_tracer("probe").start_as_current_span("inbound", context=ctx):
            print(call(target))
