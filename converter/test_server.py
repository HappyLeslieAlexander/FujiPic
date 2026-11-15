"""Protocol/admission regression; standard library only, no native decoding."""
import http.client
from pathlib import Path
import socket
import tempfile
import threading
import unittest
import server


class UnixConnection(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(2)
        self.sock.connect(self.host)


class AdmissionTests(unittest.TestCase):
    def test_busy_request_is_not_queued_behind_decoder(self):
        started, release = threading.Event(), threading.Event()
        class Blocking(server.Handler):
            def process(self):
                started.set()
                release.wait(5)
                self.send_response(200)
                self.send_header('Content-Length', '0')
                self.end_headers()
        with tempfile.TemporaryDirectory() as tmp:
            path = str(Path(tmp) / 's')
            with server.Server(path, Blocking) as service:
                thread = threading.Thread(target=service.serve_forever)
                thread.start()
                first, second = UnixConnection(path), UnixConnection(path)
                try:
                    first.request('POST', '/inspect', b'x')
                    self.assertTrue(started.wait(2))
                    second.request('POST', '/convert', b'x')
                    response = second.getresponse()
                    self.assertEqual(response.status, 503)
                    self.assertEqual(response.getheader('X-Media-Busy'), '1')
                    release.set()
                    self.assertEqual(first.getresponse().status, 200)
                finally:
                    release.set()
                    first.close()
                    second.close()
                    service.shutdown()
                    thread.join()


if __name__ == '__main__':
    unittest.main()
