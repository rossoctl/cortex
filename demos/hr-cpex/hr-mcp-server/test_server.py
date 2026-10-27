"""Checks that hr-mcp reads a request the way the sidecar's policy did.

Run: python3 -m unittest -q test_server  (needs fastapi, as server.py does)
"""

import unittest

import server


def _adjust(amount):
    server.EMPLOYEES["EMP-001234"]["salary"] = 125000
    return server.tool_adjust_compensation({"employee_id": "EMP-001234", "amount": amount})


class ParseBody(unittest.TestCase):
    def test_refuses_what_the_sidecar_cannot_parse(self):
        for literal in ("NaN", "Infinity", "-Infinity", "1e400", "-1e400"):
            body = b'{"method":"tools/call","params":{"arguments":{"amount":%s}}}' % literal.encode()
            with self.assertRaises(ValueError, msg=literal):
                server.parse_body(body)

    def test_refuses_duplicate_keys(self):
        for body in (
            b'{"method":"tools/call","method":"tools/list"}',
            b'{"params":{"arguments":{"amount":5000,"amount":50000}}}',
            b'{"params":{"arguments":{"opts":{"k":1,"k":2}}}}',
        ):
            with self.assertRaises(ValueError, msg=body):
                server.parse_body(body)

    def test_refuses_case_variant_keys_where_the_sidecar_does(self):
        for body in (
            b'{"Method":"x","method":"tools/call"}',
            b'{"params":{"name":"a","Name":"b"}}',
            b'{"params":{"arguments":{"amount":5000,"Amount":50000}}}',
            b'{"params":{"arguments":{"list":[{"Path":"/a","path":"/b"}]}}}',
        ):
            with self.assertRaises(ValueError, msg=body):
                server.parse_body(body)
        # Keys elsewhere are data, as they are to the sidecar.
        server.parse_body(b'{"method":"tools/call","meta":{"ID":1,"id":2}}')

    def test_refuses_lone_surrogates(self):
        for body in (b'{"a":"x\\ud800"}', b'{"a":"\\udc00"}', b'{"\\ud800":1}'):
            with self.assertRaises(ValueError, msg=body):
                server.parse_body(body)
        self.assertEqual(server.parse_body(b'{"a":"\\ud83d\\ude00"}'), {"a": "\U0001f600"})

    def test_refuses_what_is_not_utf8(self):
        body = '{"method":"tools/call"}'
        for encoded in (
            body.encode("utf-16-be"),
            body.encode("utf-16"),
            body.encode("utf-32"),
            b"\xef\xbb\xbf" + body.encode(),
        ):
            with self.assertRaises(ValueError, msg=encoded[:4]):
                server.parse_body(encoded)

    def test_refuses_trailing_data(self):
        for body in (b'{"a":1} {"b":2}', b'{"a":1}x'):
            with self.assertRaises(ValueError, msg=body):
                server.parse_body(body)
        self.assertEqual(server.parse_body(b'{"a":1}\n'), {"a": 1})

    def test_reads_numbers_as_json_does(self):
        rpc = server.parse_body(b'{"a":15000,"b":15000.0,"c":1.5e4,"d":-0,"e":9007199254740993}')
        self.assertEqual(rpc, {"a": 15000, "b": 15000.0, "c": 15000.0, "d": 0, "e": 9007199254740993})


class ParseRPC(unittest.TestCase):
    def test_refuses_what_is_not_json_rpc(self):
        # hr-mcp speaks JSON-RPC 2.0 only; anything else is not a call,
        # including a body Go could not read (a 400-digit integer, say).
        call = b'"method":"tools/call","params":{"name":"get_compensation","arguments":{"employee_id":"EMP-001234"}}'
        for body in (
            b"{" + call + b"}",
            b'{"jsonrpc":"1.0",' + call + b"}",
            b'{"JSONRPC":"2.0",' + call + b"}",
            b'{"x":1' + b"0" * 400 + b"," + call + b"}",
            b"[{" + call + b"}]",
        ):
            with self.assertRaises(ValueError, msg=body[:20]):
                server.parse_rpc(body)
        rpc = server.parse_rpc(b'{"jsonrpc":"2.0",' + call + b"}")
        self.assertEqual(rpc["method"], "tools/call")


class AdjustAmount(unittest.TestCase):
    def test_applies_whole_dollar_numbers(self):
        for amount, want in ((5000, 5000), (5000.0, 5000), (1.5e4, 15000), (-0.0, 0), (-25000, -25000), (2**53, 2**53)):
            out = _adjust(amount)
            self.assertEqual(out.get("adjustment"), want, amount)
            self.assertIsInstance(out["adjustment"], int)

    def test_refuses_what_the_threshold_did_not_see(self):
        # Past 2^53 the policy's float64 is not the integer applied here.
        too_big = (2**53 + 1, -(2**53 + 1), 1e300)
        for amount in ("15000", True, False, None, [15000], {"v": 1}, 10000.5, float("nan"), float("inf"), *too_big):
            self.assertIn("error", _adjust(amount), repr(amount))


if __name__ == "__main__":
    unittest.main()
