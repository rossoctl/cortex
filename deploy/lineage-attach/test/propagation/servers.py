"""One server per framework on :8000 whose handler makes an outbound
`requests` call to SINK and returns the sink's echo, so the inbound
traceparent (from driver.py) must be extracted by the framework and
re-injected by requests to reach the sink. Usage: servers.py <row>
Imports stay inside each function: a probe image carries one framework.
The module doubles as the Django settings module (DJANGO_SETTINGS_MODULE=servers),
so its top level is constants only; Django's urlconf is this process's __main__."""

import os
import sys

# Django settings (read only on the django row, at interpreter start by the hook).
SECRET_KEY = "probe"
DEBUG = False
ALLOWED_HOSTS = ["*"]
ROOT_URLCONF = "__main__"
MIDDLEWARE = []
INSTALLED_APPS = []


def fetch():
    import requests

    return requests.get(os.environ["SINK"], timeout=5).text


def starlette_():
    import uvicorn
    from starlette.applications import Starlette
    from starlette.responses import PlainTextResponse
    from starlette.routing import Route

    app = Starlette(routes=[Route("/", lambda r: PlainTextResponse(fetch()))])
    uvicorn.run(app, host="0.0.0.0", port=8000, log_level="error")


def fastapi_():
    import uvicorn
    from fastapi import FastAPI
    from fastapi.responses import PlainTextResponse

    app = FastAPI()
    app.get("/", response_class=PlainTextResponse)(fetch)
    uvicorn.run(app, host="0.0.0.0", port=8000, log_level="error")


def aiohttp_server():
    from aiohttp import web

    async def handle(_):
        return web.Response(text=fetch())

    app = web.Application()
    app.router.add_get("/", handle)
    web.run_app(app, host="0.0.0.0", port=8000, print=None)


def flask_():
    from flask import Flask

    app = Flask(__name__)
    app.get("/")(fetch)
    app.run(host="0.0.0.0", port=8000)


def django_():
    from django.core.management import execute_from_command_line
    from django.http import HttpResponse
    from django.urls import path

    globals()["urlpatterns"] = [path("", lambda r: HttpResponse(fetch()))]
    execute_from_command_line([sys.argv[0], "runserver", "0.0.0.0:8000", "--noreload", "--skip-checks"])


def falcon_():
    from wsgiref.simple_server import make_server

    import falcon

    class Root:
        def on_get(self, req, resp):
            resp.text = fetch()

    app = falcon.App()
    app.add_route("/", Root())
    make_server("0.0.0.0", 8000, app).serve_forever()


def pyramid_():
    from wsgiref.simple_server import make_server

    from pyramid.config import Configurator
    from pyramid.response import Response

    with Configurator() as config:
        config.add_route("root", "/")
        config.add_view(lambda r: Response(fetch()), route_name="root")
        app = config.make_wsgi_app()
    make_server("0.0.0.0", 8000, app).serve_forever()


def tornado_():
    import tornado.ioloop
    import tornado.web

    class Root(tornado.web.RequestHandler):
        def get(self):
            self.write(fetch())

    tornado.web.Application([("/", Root)]).listen(8000)
    tornado.ioloop.IOLoop.current().start()


def grpc_():
    """Server half of the gRPC row: a generic unary handler that reports the
    traceparent it received in metadata and the one the sink saw on its own
    outbound requests call. The client half is clients.py grpc."""
    import json
    from concurrent import futures

    import grpc

    ident = lambda b: b  # noqa: E731  raw bytes, no proto

    def echo(request, context):
        md = {k: v for k, v in context.invocation_metadata()}
        return json.dumps({"inbound": md.get("traceparent"), "sink": json.loads(fetch())}).encode()

    handler = grpc.method_handlers_generic_handler(
        "probe.Echo", {"Echo": grpc.unary_unary_rpc_method_handler(echo, ident, ident)}
    )
    server = grpc.server(futures.ThreadPoolExecutor(2))
    server.add_generic_rpc_handlers((handler,))
    server.add_insecure_port("0.0.0.0:8000")
    server.start()
    server.wait_for_termination()


SERVERS = {
    "starlette": starlette_,
    "fastapi": fastapi_,
    "aiohttp_server": aiohttp_server,
    "flask": flask_,
    "django": django_,
    "falcon": falcon_,
    "pyramid": pyramid_,
    "tornado": tornado_,
    "grpc": grpc_,
}

if __name__ == "__main__":
    SERVERS[sys.argv[1]]()
