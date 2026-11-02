"""Call a probe server with a chosen traceparent (retrying until it is up)
and print its body."""

import sys
import time
import urllib.request

url, traceparent = sys.argv[1], sys.argv[2]
req = urllib.request.Request(url, headers={"traceparent": traceparent})
for attempt in range(60):
    try:
        print(urllib.request.urlopen(req, timeout=5).read().decode())
        break
    except Exception:  # noqa: BLE001  server not up yet
        time.sleep(0.5)
else:
    sys.exit("probe server never answered")
