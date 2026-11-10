#!/usr/bin/env python3
"""Run the upload regression in real headless Chromium, without Node packages."""

import functools
import html.parser
import http.server
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading


class ResultParser(html.parser.HTMLParser):
    def __init__(self):
        super().__init__()
        self.state = None
        self.in_result = False
        self.text = []

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "html":
            self.state = attrs.get("data-result")
        if tag == "pre" and attrs.get("id") == "result":
            self.in_result = True

    def handle_endtag(self, tag):
        if tag == "pre":
            self.in_result = False

    def handle_data(self, data):
        if self.in_result:
            self.text.append(data)


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *_args):
        pass


def main():
    chrome = os.environ.get("CHROME_BIN")
    if not chrome:
        for name in ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser"):
            chrome = shutil.which(name)
            if chrome:
                break
    if not chrome:
        mac_chrome = Path("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
        if mac_chrome.is_file():
            chrome = str(mac_chrome)
    if not chrome:
        raise SystemExit("Chrome/Chromium not found; set CHROME_BIN to its executable")

    root = Path(__file__).resolve().parent.parent
    handler = functools.partial(QuietHandler, directory=str(root))
    with http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler) as server:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="fujipic-browser-") as profile:
                completed = subprocess.run(
                    [chrome, "--headless", "--disable-gpu", "--no-first-run",
                     f"--user-data-dir={profile}", "--dump-dom", "--virtual-time-budget=10000",
                     f"http://127.0.0.1:{server.server_port}/scripts/browser-upload-test.html"],
                    capture_output=True, text=True, timeout=30,
                )
                result = ResultParser()
                result.feed(completed.stdout)
                if completed.returncode != 0 or result.state != "pass":
                    raise SystemExit("Browser regression failed:\n" + "".join(result.text)
                                     + "\n" + completed.stderr[-4000:])
                print("".join(result.text))
        finally:
            server.shutdown()
            thread.join()


if __name__ == "__main__":
    main()
