"""4AiCoding Portal SDK — Quickstart snippet.

Source of truth for the Quickstart page on the Portal UI. Runs
end-to-end against a real Portal: create a sandbox, exec `echo 1+1`,
read container logs, then delete.

Usage:
    PORTAL_API_KEY=sbx_live_xxx python examples/quickstart.py
"""

import os
import sys
import time

from portal_sdk import PortalClient


def main() -> int:
    # 1. Configure the client (Portal API key `sbx_live_...`).
    api_key = os.environ.get("PORTAL_API_KEY", "sbx_live_REPLACE_ME")
    base_url = os.environ.get("PORTAL_BASE_URL", "https://chat.devhub.io.vn")
    client = PortalClient(base_url=base_url, api_key=api_key, timeout=30)

    # 2. Create a sandbox.
    sb = client.create_sandbox(image="ubuntu:24.04")
    print(f"created sandbox: id={sb.id} status={sb.status}")

    try:
        # 3. Exec a command (buffered).
        result = client.exec(sb.id, cmd=["sh", "-c", "echo $((1+1))"])
        print(f"exec: exit={result.exit_code} stdout={result.stdout!r}")

        # 4. Stream exec output as NDJSON.
        print("streaming exec output:")
        for chunk in client.exec_stream(
            sb.id,
            cmd=["sh", "-c", "for i in 1 2 3; do echo line $i; done"],
            timeout_s=10,
        ):
            if "stream" in chunk:
                print(f"  {chunk['stream']}: {chunk['data']!r}")
            elif "exit_code" in chunk:
                print(
                    f"  exit_code={chunk['exit_code']} "
                    f"duration_ms={chunk.get('duration_ms')}"
                )

        # 5. Read container logs as text (no follow).
        logs = client.get_logs(sb.id, tail=50).decode("utf-8")
        print(f"logs (last 50 lines):\n{logs}")

        # 6. Follow logs as SSE (just for ~1s in this demo).
        print("following logs (1s)…")
        start = time.time()
        for line in client.logs_follow(sb.id, tail=10):
            print(f"  sse: {line}")
            if time.time() - start > 1:
                break
    finally:
        # 7. Delete the sandbox.
        client.delete_sandbox(sb.id)
        print(f"deleted sandbox: id={sb.id}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
