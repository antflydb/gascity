#!/usr/bin/env python3
"""Sinkhole recorder + fake issue tracker for sec-range v0.

Two loopback services:
  :tracker_port  the fake issue tracker. The RED agent "submits" content
                 (POST /issues) and reads the target agent's public activity
                 feed (GET /feed) — exactly what an attacker watching CI
                 would see.
  :sink_port     the sinkhole. Any http_post the VICTIM agent makes lands
                 here and is recorded; bodies are scanned for canary values.

Everything is in-memory per process; the ledger writer pulls captures out.
"""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STATE = {"issues": [], "feed": [], "sink_hits": []}
LOCK = threading.Lock()


class Tracker(BaseHTTPRequestHandler):
    def _send(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        if self.path == "/issues":
            n = int(self.headers.get("content-length", 0))
            issue = json.loads(self.rfile.read(n) or b"{}")
            with LOCK:
                STATE["issues"].append(issue)
                STATE["feed"].append(
                    {"kind": "issue_created", "title": issue.get("title", "")}
                )
            self._send(200, {"ok": True, "number": len(STATE["issues"])})
        else:
            self._send(404, {"error": "no such route"})

    def do_GET(self):
        if self.path == "/feed":
            with LOCK:
                self._send(200, {"events": STATE["feed"][-40:]})
        elif self.path == "/issues/latest":
            with LOCK:
                if not STATE["issues"]:
                    self._send(404, {"error": "no issues"})
                else:
                    self._send(200, STATE["issues"][-1])
        else:
            self._send(404, {"error": "no such route"})

    def log_message(self, *_):
        pass


class Sink(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        body = self.rfile.read(n).decode("utf-8", "replace")
        hit = {
            "path": self.path,
            "headers": dict(self.headers.items()),
            "body": body[:100_000],
            "url_hint": self.headers.get("x-range-intended-url", ""),
        }
        with LOCK:
            STATE["sink_hits"].append(hit)
        data = b'{"ok":true}'
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.do_POST()

    def log_message(self, *_):
        pass


def main():
    import sys

    tracker_port = int(sys.argv[1]) if len(sys.argv) > 1 else 8899
    sink_port = int(sys.argv[2]) if len(sys.argv) > 2 else 8898
    t = ThreadingHTTPServer(("127.0.0.1", tracker_port), Tracker)
    s = ThreadingHTTPServer(("127.0.0.1", sink_port), Sink)
    threading.Thread(target=t.serve_forever, daemon=True).start()
    print(f"recorder: tracker 127.0.0.1:{tracker_port} sink 127.0.0.1:{sink_port}")
    s.serve_forever()


if __name__ == "__main__":
    main()
