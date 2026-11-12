"""End-to-end tests for the Portal Python SDK against a stdlib-only
fake HTTP server (http.server.ThreadingHTTPServer).

Runs cleanly via:
    cd portal/sdk/python
    python3 -m pytest test_portal_sdk.py
or:
    python3 -m unittest test_portal_sdk

No third-party deps — only the standard library.
"""

import hashlib
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

from portal_sdk import PortalClient, PortalError
from portal_sdk.models import (
    ExecResult,
    FileContent,
    FileListing,
    FileWriteResult,
    Sandbox,
    UsageResponse,
)

# 26-char ULID (no `sbx-` prefix — per review #52 contract).
SANDBOX_ID = "01HGE2K3Z9Q5P7R8T0V1X2Y3Z4"
AUTH_TOKEN = "sbx_live_test_token"


class FakePortalHandler(BaseHTTPRequestHandler):
    # HTTP/1.0 default → connection closes after each response,
    # which lets us stream NDJSON / SSE by writing then closing.
    protocol_version = "HTTP/1.0"

    # Silence stderr logging.
    def log_message(self, *args, **kwargs):  # noqa: D401
        pass

    # ----- helpers ----------------------------------------------------

    def _read_body(self) -> bytes:
        length = int(self.headers.get("Content-Length", "0"))
        if length == 0:
            return b""
        return self.rfile.read(length)

    def _send_json(self, status: int, obj, extra_headers=None) -> None:
        body = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        if extra_headers:
            for k, v in extra_headers.items():
                self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def _send_no_content(self) -> None:
        self.send_response(204)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def _send_text(self, status: int, text: str, content_type="text/plain") -> None:
        body = text.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _auth_ok(self) -> bool:
        return self.headers.get("Authorization", "") == f"Bearer {self.server.api_key}"  # type: ignore[attr-defined]

    def _unauthorized(self) -> None:
        self._send_json(
            401,
            {"error": {"code": "unauthenticated", "message": "missing token"}},
            extra_headers={"Retry-After": "1"},
        )

    # ----- GET --------------------------------------------------------

    def do_GET(self) -> None:  # noqa: C901
        if not self._auth_ok():
            self._unauthorized()
            return
        parsed = urlparse(self.path)
        path = parsed.path
        qs = parse_qs(parsed.query)
        srv = self.server  # type: ignore[attr-defined]

        if path == "/v1/sandboxes":
            self._send_json(
                200,
                {
                    "sandboxes": [
                        {"id": SANDBOX_ID, "status": "running", "image": "ubuntu:24.04"}
                    ],
                    "unreachable_nodes": [],
                },
            )
            return

        if path == f"/v1/sandboxes/{SANDBOX_ID}":
            self._send_json(
                200,
                {
                    "id": SANDBOX_ID,
                    "status": "running",
                    "image": "ubuntu:24.04",
                    "created_at": "2024-01-01T00:00:00Z",
                    "updated_at": "2024-01-01T00:00:00Z",
                },
            )
            return

        if path == f"/v1/sandboxes/{SANDBOX_ID}/files":
            self._send_json(
                200,
                {
                    "path": qs.get("path", [""])[0],
                    "recursive": qs.get("recursive", ["false"])[0] == "true",
                    "entries": [
                        {"path": "hello.txt", "name": "hello.txt", "size": 5, "is_dir": False}
                    ],
                    "truncated": False,
                },
            )
            return

        if path == f"/v1/sandboxes/{SANDBOX_ID}/files/content":
            file_path = qs.get("path", [""])[0]
            content = srv.files.get(file_path, b"")  # type: ignore[attr-defined]
            self._send_json(
                200,
                {
                    "path": file_path,
                    "size": len(content),
                    "offset": 0,
                    "encoding": "utf-8",
                    "data": content.decode("utf-8"),
                    "truncated": False,
                },
            )
            return

        if path == f"/v1/sandboxes/{SANDBOX_ID}/logs":
            follow = qs.get("follow", ["false"])[0] == "true"
            if follow:
                # SSE: write data: lines, then end (close connection).
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(b"data: line1\n\n")
                self.wfile.write(b"data: line2\n\n")
                self.wfile.flush()
                return
            self._send_text(200, "log line 1\nlog line 2\n")
            return

        if path == "/v1/usage":
            self._send_json(
                200,
                {
                    "from": "2024-01-01T00:00:00Z",
                    "to": "2024-01-02T00:00:00Z",
                    "kind": "all",
                    "series": [
                        {
                            "day": "2024-01-01T00:00:00Z",
                            "kind": "sandbox_seconds",
                            "quantity": 3600,
                        }
                    ],
                    "plan": {
                        "plan_id": "free",
                        "plan_name": "Free",
                        "monthly_vcpu_seconds": 100000,
                        "used_vcpu_seconds": 3600,
                        "max_concurrent_sandboxes": 3,
                        "running_sandboxes": 1,
                    },
                },
            )
            return

        self._send_json(404, {"error": {"code": "not_found", "message": path}})

    # ----- POST ------------------------------------------------------

    def do_POST(self) -> None:  # noqa: C901
        if not self._auth_ok():
            self._unauthorized()
            return
        body = self._read_body()
        parsed = urlparse(self.path)
        path = parsed.path
        qs = parse_qs(parsed.query)
        srv = self.server  # type: ignore[attr-defined]

        if path == "/v1/sandboxes":
            idem = self.headers.get("Idempotency-Key")
            if idem and idem in srv.idempotency_cache:  # type: ignore[attr-defined]
                self._send_json(201, srv.idempotency_cache[idem])  # type: ignore[attr-defined]
                return
            srv.create_count += 1  # type: ignore[attr-defined]
            data = json.loads(body) if body else {}
            sandbox = {
                "id": SANDBOX_ID,
                "status": "running",
                "image": data.get("image", "ubuntu:24.04"),
                "created_at": "2024-01-01T00:00:00Z",
                "updated_at": "2024-01-01T00:00:00Z",
            }
            if idem:
                srv.idempotency_cache[idem] = sandbox  # type: ignore[attr-defined]
            self._send_json(201, sandbox)
            return

        if path == "/v1/tokens":
            self._send_json(
                200,
                {"token": "agent_tok_test", "expires_at": "2024-01-01T01:00:00Z"},
            )
            return

        if path == f"/v1/sandboxes/{SANDBOX_ID}/start":
            self._send_no_content()
            return
        if path == f"/v1/sandboxes/{SANDBOX_ID}/stop":
            self._send_no_content()
            return

        if path == f"/v1/sandboxes/{SANDBOX_ID}/exec":
            stream = qs.get("stream", [None])[0]
            if stream == "ndjson":
                self.send_response(200)
                self.send_header("Content-Type", "application/x-ndjson")
                self.end_headers()
                self.wfile.write(b'{"stream":"stdout","data":"A\\n"}\n')
                self.wfile.write(b'{"stream":"stdout","data":"A\\n"}\n')
                self.wfile.write(
                    b'{"exit_code":0,"timed_out":false,"duration_ms":12}\n'
                )
                self.wfile.flush()
                return
            self._send_json(
                200,
                {
                    "stdout": "hi\n",
                    "stderr": "",
                    "exit_code": 0,
                    "timed_out": False,
                    "truncated": False,
                    "duration_ms": 5,
                },
            )
            return

        if path == f"/v1/sandboxes/{SANDBOX_ID}/files/move":
            data = json.loads(body) if body else {}
            self._send_json(
                200, {"from": data.get("from", ""), "to": data.get("to", "")}
            )
            return

        self._send_json(404, {"error": {"code": "not_found", "message": path}})

    # ----- PUT -------------------------------------------------------

    def do_PUT(self) -> None:
        if not self._auth_ok():
            self._unauthorized()
            return
        body = self._read_body()
        parsed = urlparse(self.path)
        path = parsed.path
        qs = parse_qs(parsed.query)
        srv = self.server  # type: ignore[attr-defined]

        if path == f"/v1/sandboxes/{SANDBOX_ID}/files/content":
            file_path = qs.get("path", [""])[0]
            srv.files[file_path] = body  # type: ignore[attr-defined]
            sha = hashlib.sha256(body).hexdigest()
            self._send_json(
                200,
                {"path": file_path, "size": len(body), "sha256": sha},
            )
            return

        self._send_json(404, {"error": {"code": "not_found", "message": path}})

    # ----- DELETE ----------------------------------------------------

    def do_DELETE(self) -> None:
        if not self._auth_ok():
            self._unauthorized()
            return
        parsed = urlparse(self.path)
        path = parsed.path
        qs = parse_qs(parsed.query)
        srv = self.server  # type: ignore[attr-defined]

        if path == f"/v1/sandboxes/{SANDBOX_ID}":
            self._send_no_content()
            return
        if path == f"/v1/sandboxes/{SANDBOX_ID}/files":
            file_path = qs.get("path", [""])[0]
            srv.files.pop(file_path, None)  # type: ignore[attr-defined]
            self._send_no_content()
            return

        self._send_json(404, {"error": {"code": "not_found", "message": path}})


class TestPortalSDK(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), FakePortalHandler)
        cls.server.api_key = AUTH_TOKEN  # type: ignore[attr-defined]
        cls.server.idempotency_cache = {}  # type: ignore[attr-defined]
        cls.server.create_count = 0  # type: ignore[attr-defined]
        cls.server.files = {}  # type: ignore[attr-defined]
        cls.thread = threading.Thread(
            target=cls.server.serve_forever, daemon=True
        )
        cls.thread.start()
        cls.base_url = f"http://127.0.0.1:{cls.server.server_address[1]}"

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self) -> None:
        self.server.idempotency_cache.clear()  # type: ignore[attr-defined]
        self.server.create_count = 0  # type: ignore[attr-defined]
        self.server.files.clear()  # type: ignore[attr-defined]
        self.client = PortalClient(
            base_url=self.base_url, api_key=AUTH_TOKEN, timeout=5
        )

    # ------------------------------------------------------------------
    # Sandboxes
    # ------------------------------------------------------------------

    def test_create_sandbox(self) -> None:
        sb = self.client.create_sandbox(image="ubuntu:24.04")
        self.assertIsInstance(sb, Sandbox)
        self.assertEqual(sb.id, SANDBOX_ID)
        self.assertEqual(sb.image, "ubuntu:24.04")
        self.assertEqual(sb.status, "running")

    def test_create_sandbox_auto_sends_idempotency_key(self) -> None:
        # Without an explicit idempotency_key, the SDK should generate
        # a UUID4 and send it as the Idempotency-Key header. The fake
        # server populates its cache when it sees the header.
        sb = self.client.create_sandbox(image="ubuntu:24.04")
        self.assertEqual(sb.id, SANDBOX_ID)
        self.assertEqual(len(self.server.idempotency_cache), 1)  # type: ignore[attr-defined]

    def test_idempotency_dedup(self) -> None:
        key = "fixed-idem-key-123"
        sb1 = self.client.create_sandbox(
            image="ubuntu:24.04", idempotency_key=key
        )
        sb2 = self.client.create_sandbox(
            image="ubuntu:24.04", idempotency_key=key
        )
        self.assertEqual(sb1.id, sb2.id)
        # Server should have processed the create only once.
        self.assertEqual(self.server.create_count, 1)  # type: ignore[attr-defined]

    def test_get_sandbox(self) -> None:
        sb = self.client.get_sandbox(SANDBOX_ID)
        self.assertEqual(sb.id, SANDBOX_ID)
        self.assertEqual(sb.status, "running")

    def test_list_sandboxes(self) -> None:
        result = self.client.list_sandboxes()
        self.assertEqual(len(result["sandboxes"]), 1)
        self.assertIsInstance(result["sandboxes"][0], Sandbox)
        self.assertEqual(result["sandboxes"][0].id, SANDBOX_ID)
        self.assertEqual(result["unreachable_nodes"], [])

    def test_start_stop_delete(self) -> None:
        # All three return 204 — assertions are simply "no exception".
        self.client.start_sandbox(SANDBOX_ID)
        self.client.stop_sandbox(SANDBOX_ID)
        self.client.delete_sandbox(SANDBOX_ID)

    # ------------------------------------------------------------------
    # Exec
    # ------------------------------------------------------------------

    def test_exec_buffered(self) -> None:
        result = self.client.exec(SANDBOX_ID, cmd=["echo", "hi"])
        self.assertIsInstance(result, ExecResult)
        self.assertEqual(result.exit_code, 0)
        self.assertEqual(result.stdout, "hi\n")
        self.assertFalse(result.timed_out)

    def test_exec_stream(self) -> None:
        events = list(
            self.client.exec_stream(
                SANDBOX_ID, cmd=["yes", "A"], timeout_s=2
            )
        )
        stdout_events = [e for e in events if "stream" in e]
        exit_events = [e for e in events if "exit_code" in e]
        self.assertGreaterEqual(len(stdout_events), 1)
        self.assertGreaterEqual(len(exit_events), 1)
        self.assertEqual(exit_events[0]["exit_code"], 0)

    # ------------------------------------------------------------------
    # Files
    # ------------------------------------------------------------------

    def test_write_and_read_file(self) -> None:
        write_result = self.client.write_file(
            SANDBOX_ID, path="hello.txt", data=b"hello"
        )
        self.assertIsInstance(write_result, FileWriteResult)
        self.assertEqual(write_result.size, 5)
        self.assertEqual(write_result.path, "hello.txt")
        self.assertEqual(len(write_result.sha256), 64)

        content = self.client.read_file(SANDBOX_ID, path="hello.txt")
        self.assertIsInstance(content, FileContent)
        self.assertEqual(content.data, "hello")
        self.assertEqual(content.encoding, "utf-8")

    def test_list_files(self) -> None:
        listing = self.client.list_files(SANDBOX_ID, path=".")
        self.assertIsInstance(listing, FileListing)
        self.assertEqual(listing.entries[0].path, "hello.txt")

    def test_delete_file(self) -> None:
        self.client.write_file(SANDBOX_ID, path="temp.txt", data=b"x")
        self.client.delete_file(SANDBOX_ID, path="temp.txt")
        # After delete, read should return empty bytes.
        content = self.client.read_file(SANDBOX_ID, path="temp.txt")
        self.assertEqual(content.data, "")

    def test_move_file(self) -> None:
        result = self.client.move_file(
            SANDBOX_ID, frm="a.txt", to="b.txt"
        )
        self.assertEqual(result["from"], "a.txt")
        self.assertEqual(result["to"], "b.txt")

    # ------------------------------------------------------------------
    # Logs
    # ------------------------------------------------------------------

    def test_logs_text(self) -> None:
        data = self.client.get_logs(SANDBOX_ID, tail=10, follow=False)
        self.assertIn(b"log line 1", data)
        self.assertIn(b"log line 2", data)

    def test_logs_follow(self) -> None:
        events = list(self.client.logs_follow(SANDBOX_ID))
        self.assertGreaterEqual(len(events), 1)
        self.assertIn("line1", events[0])

    # ------------------------------------------------------------------
    # Usage + Tokens
    # ------------------------------------------------------------------

    def test_get_usage(self) -> None:
        usage = self.client.get_usage()
        self.assertIsInstance(usage, UsageResponse)
        self.assertEqual(usage.plan.plan_name, "Free")
        self.assertEqual(len(usage.series), 1)
        self.assertEqual(usage.series[0].quantity, 3600)

    def test_create_token(self) -> None:
        result = self.client.create_token(scopes=["sandboxes:read"])
        self.assertEqual(result["token"], "agent_tok_test")
        self.assertEqual(result["expires_at"], "2024-01-01T01:00:00Z")

    # ------------------------------------------------------------------
    # Error path
    # ------------------------------------------------------------------

    def test_unauthorized_raises_portal_error(self) -> None:
        bad_client = PortalClient(
            base_url=self.base_url, api_key="wrong", timeout=5
        )
        with self.assertRaises(PortalError) as ctx:
            bad_client.get_sandbox(SANDBOX_ID)
        self.assertEqual(ctx.exception.status_code, 401)
        self.assertEqual(ctx.exception.code, "unauthenticated")


if __name__ == "__main__":
    unittest.main()
