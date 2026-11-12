#!/usr/bin/env python3
"""Exercise the Cloudflare Caddyfile with native Caddy; no Docker/cloud access.

Requires openssl and CADDY_BIN (or caddy on PATH). Uses ephemeral ports,
self-signed test TLS, a local HTTP backend, and temporary configuration only.
"""

import argparse
import http.client
import http.server
import os
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time


ROOT = Path(__file__).resolve().parent.parent


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Backend(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.server.requests.append((self.path, self.headers))
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"backend")

    do_HEAD = do_GET

    def log_message(self, *_):
        pass


def request(port, host, path, method="GET", headers=None):
    # Certificate verification is disabled ONLY for generated self-signed fixtures.
    context = ssl._create_unverified_context()
    with socket.create_connection(("127.0.0.1", port), timeout=3) as raw:
        with context.wrap_socket(raw, server_hostname=host) as conn:
            fields = {"Host": host, "Connection": "close", **(headers or {})}
            message = f"{method} {path} HTTP/1.1\r\n"
            message += "".join(f"{key}: {value}\r\n" for key, value in fields.items())
            conn.sendall((message + "\r\n").encode())
            response = http.client.HTTPResponse(conn, method=method)
            response.begin()
            body = response.read()
            return response.status, body


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", action="store_true", help="Test host Caddy site blocks instead")
    args = parser.parse_args()
    caddy = os.environ.get("CADDY_BIN") or shutil.which("caddy")
    if not caddy:
        raise SystemExit("Set CADDY_BIN to a native Caddy binary (2.11.9 or newer).")
    hosts = ("pic.example.test", "img.example.test", "alias.example.test") if args.host else ("pic.example.test", "img.example.test")
    backend = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Backend)
    backend.requests = []
    thread = threading.Thread(target=backend.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="fujipic-caddy-") as directory:
            tmp = Path(directory)
            subprocess.run([
                "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                "-keyout", str(tmp / "key.pem"), "-out", str(tmp / "cert.pem"),
                "-days", "1", "-subj", f"/CN={hosts[0]}", "-addext",
                "subjectAltName=" + ",".join("DNS:" + h for h in hosts),
            ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            for allow_loopback in (False, True):
                port = free_port()
                ranges = (ROOT / "deploy/cloudflare-ips.caddy").read_text()
                if allow_loopback:
                    ranges = "@not_cloudflare {\n not remote_ip 127.0.0.1/32\n}\n"
                (tmp / "ips.caddy").write_text(ranges)
                template = "deploy/Caddyfile.cloudflare-host" if args.host else "deploy/Caddyfile.cloudflare"
                config = (ROOT / template).read_text()
                if args.host:
                    config += "\n" + (ROOT / "deploy/Caddyfile.image-alias").read_text()
                    config = config.replace("{$IMAGE_ALIAS_HOST}", hosts[2])
                    config = config.replace("{$IMAGE_ALIAS_CERT}", str(tmp / "cert.pem"))
                    config = config.replace("{$IMAGE_ALIAS_KEY}", str(tmp / "key.pem"))
                    config = "{\n admin off\n auto_https off\n}\n" + config
                    config = config.replace("/etc/caddy/fujipic-cloudflare-ips.caddy", str(tmp / "ips.caddy"))
                    config = config.replace("/etc/caddy/fujipic-origin/", str(tmp) + "/")
                    config = config.replace("127.0.0.1:18080", f"127.0.0.1:{backend.server_port}")
                    config = config.replace(":443 {", f":{port} {{")
                config = config.replace("/etc/caddy/cloudflare-ips.caddy", str(tmp / "ips.caddy"))
                config = config.replace("/etc/caddy/origin/", str(tmp) + "/")
                config = config.replace("app:8080", f"127.0.0.1:{backend.server_port}")
                config = config.replace(":8443", f":{port}")
                config = config.replace("{$APP_HOST}", hosts[0]).replace("{$IMAGE_HOST}", hosts[1])
                (tmp / "Caddyfile").write_text(config)
                with (tmp / "caddy.log").open("w+") as log:
                    process = subprocess.Popen([
                        caddy, "run", "--config", str(tmp / "Caddyfile"), "--adapter", "caddyfile",
                    ], stdout=log, stderr=log, env={**os.environ, "XDG_DATA_HOME": str(tmp / "data"),
                                                  "XDG_CONFIG_HOME": str(tmp / "config")})
                    try:
                        for _ in range(100):
                            if process.poll() is not None:
                                log.seek(0)
                                raise AssertionError(log.read())
                            try:
                                request(port, hosts[0], "/login")
                                break
                            except (OSError, http.client.HTTPException):
                                time.sleep(0.05)
                        else:
                            raise AssertionError("Caddy did not become ready")
                        backend.requests.clear()
                        headers = {"CF-Connecting-IP": "198.51.100.7", "Cookie": "session=test",
                                   "Authorization": "Bearer test", "X-Origin-Secret": "edge-secret"}
                        if not allow_loopback:
                            for host, path in ((hosts[0], "/healthz"), (hosts[0], "/login"),
                                               (hosts[1], "/i/test.png")):
                                status, _ = request(port, host, path, headers=headers)
                                assert status == 403, (host, path, status)
                            assert not backend.requests, "Untrusted peer reached backend"
                            if args.host:
                                assert request(port, hosts[2], "/i/test.png", headers=headers)[0] == 403
                                assert not backend.requests
                        else:
                            for path in ("/login", "/api/v1/me", "/static/app.js"):
                                assert request(port, hosts[0], path, headers=headers)[0] == 200
                                seen = backend.requests[-1][1]
                                assert seen.get("Cookie") == "session=test"
                                assert seen.get("Authorization") == "Bearer test"
                            for method in ("GET", "HEAD"):
                                assert request(port, hosts[1], "/i/test.png", method, headers)[0] == 200
                                seen = backend.requests[-1][1]
                                assert seen.get("Cookie") is None
                                assert seen.get("Authorization") is None
                                assert seen.get("CF-Connecting-IP") == "198.51.100.7"
                                assert seen.get("X-Origin-Secret") == "edge-secret"
                            for path in ("/login", "/admin", "/api/v1/me", "/healthz"):
                                count = len(backend.requests)
                                assert request(port, hosts[1], path, headers=headers)[0] == 404
                                assert len(backend.requests) == count
                            assert request(port, hosts[1], "/i/test.png", "POST", headers)[0] == 404
                            if args.host:
                                for method in ("GET", "HEAD"):
                                    assert request(port, hosts[2], "/i/test.png", method, headers)[0] == 200
                                    seen = backend.requests[-1][1]
                                    assert seen.get("Cookie") is None and seen.get("Authorization") is None
                                    assert seen.get("X-Origin-Secret") == "edge-secret"
                                for path in ("/login", "/admin", "/api/v1/me"):
                                    count = len(backend.requests)
                                    assert request(port, hosts[2], path, headers=headers)[0] == 404
                                    assert len(backend.requests) == count
                                assert request(port, hosts[2], "/i/test.png", "POST", headers)[0] == 404
                            request(port, hosts[0], "/login")
                            assert backend.requests[-1][1].get("X-Origin-Secret") is None
                            count = len(backend.requests)
                            try:
                                assert request(port, "unknown.example.test", "/login")[0] != 200
                            except ssl.SSLError:
                                pass  # Rejecting unknown SNI at TLS is also correct.
                            assert len(backend.requests) == count
                    finally:
                        process.terminate()
                        process.wait(timeout=10)
        print("PASS: TLS, source-IP gate, dual hosts, image-only routing, header isolation; no Docker/OCI/CDN exercised")
    finally:
        backend.shutdown()
        backend.server_close()
        thread.join()


if __name__ == "__main__":
    main()
