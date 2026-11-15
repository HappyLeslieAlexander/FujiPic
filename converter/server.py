"""Credential-free Unix-socket conversion service; one bounded job at a time."""
import http.server
import json
import os
from pathlib import Path
import signal
import socketserver
import subprocess
import sys
import tempfile
import threading

MAX_INPUT = int(os.environ.get('MEDIA_MAX_BYTES', 100 << 20))
MAX_PIXELS = int(os.environ.get('MEDIA_MAX_PIXELS', 50_000_000))
MAX_DIM = int(os.environ.get('MEDIA_MAX_DIMENSION', 12000))
FORMATS = {'avif', 'bmp', 'tiff', 'svg', 'raw', 'nef', 'dng', 'cr2'}
slot = threading.BoundedSemaphore(1)

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        self.connection.settimeout(30)
        if not slot.acquire(blocking=False):
            self.send_error(503)
            return
        try:
            self.process()
        except (OSError, ValueError, OverflowError):
            self.send_error(400)
        finally:
            slot.release()

    def process(self):
        if self.path not in ('/inspect', '/convert') or self.headers.get('Transfer-Encoding'):
            self.send_error(400)
            return
        size = int(self.headers.get('Content-Length', '-1'))
        fmt = self.headers.get('X-Format', '')
        pixels = min(int(self.headers.get('X-Max-Pixels', '0')), MAX_PIXELS)
        dim = min(int(self.headers.get('X-Max-Dimension', '0')), MAX_DIM)
        if not 0 < size <= MAX_INPUT or fmt not in FORMATS or pixels <= 0 or dim <= 0:
            self.send_error(415)
            return
        with tempfile.TemporaryDirectory(prefix='media-') as tmp:
            source, output, metadata = [Path(tmp) / name for name in ('input', 'output.png', 'metadata.json')]
            with source.open('wb') as file:
                remaining = size
                while remaining:
                    chunk = self.rfile.read(min(65536, remaining))
                    if not chunk:
                        raise ValueError('short body')
                    file.write(chunk)
                    remaining -= len(chunk)
            with metadata.open('wb') as meta:
                process = subprocess.Popen(
                    [sys.executable, '-I', str(Path(__file__).with_name('decode.py')), self.path[1:], str(source), fmt, str(output), str(pixels), str(dim)],
                    stdin=subprocess.DEVNULL, stdout=meta, stderr=subprocess.DEVNULL,
                    start_new_session=True,
                    env={'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'HOME': tmp, 'TMPDIR': tmp,
                         'OPENBLAS_NUM_THREADS': '1', 'OMP_NUM_THREADS': '1', 'PYTHONDONTWRITEBYTECODE': '1'},
                )
                try:
                    code = process.wait(timeout=75)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                    self.send_error(503)
                    return
            if code != 0:
                self.send_error(422 if code == 22 else 415 if code == 15 else 503)
                return
            if metadata.stat().st_size > 4096:
                self.send_error(503)
                return
            result = json.loads(metadata.read_text())
            if result.get('format') != fmt:
                self.send_error(415)
                return
            target = metadata if self.path == '/inspect' else output
            length = target.stat().st_size
            if length > 16 << 20:
                self.send_error(422)
                return
            self.send_response(200)
            self.send_header('Content-Type', 'application/json' if self.path == '/inspect' else 'image/png')
            self.send_header('Content-Length', str(length))
            self.end_headers()
            with target.open('rb') as file:
                while chunk := file.read(65536):
                    self.wfile.write(chunk)

class Server(socketserver.UnixStreamServer):
    # Sequential admission also bounds open request threads. The application
    # timeout bounds queue waits; only one decoder process can run at once.
    allow_reuse_address = True

if __name__ == '__main__':
    socket_path = os.environ.get('MEDIA_SOCKET', '/run/media/decoder.sock')
    Path(socket_path).unlink(missing_ok=True)
    os.umask(0o007)
    with Server(socket_path, Handler) as server:
        os.chmod(socket_path, 0o660)
        server.serve_forever()
