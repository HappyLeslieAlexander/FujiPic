"""Run inside the same restricted ARM64 converter image as production."""
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from PIL import Image

DECODER = str(Path(__file__).with_name('decode.py'))

class DecoderTests(unittest.TestCase):
    def decode(self, data, fmt, op='convert', pixels=50000000, dimension=12000):
        with tempfile.TemporaryDirectory() as tmp:
            source, output = Path(tmp)/'input', Path(tmp)/'output.png'
            source.write_bytes(data)
            result = subprocess.run([sys.executable, '-I', DECODER, op, str(source), fmt, str(output), str(pixels), str(dimension)], capture_output=True, timeout=85)
            return result.returncode, result.stdout, output.read_bytes() if output.exists() else b''

    def test_bitmap_formats(self):
        for fmt in ['AVIF', 'BMP', 'TIFF']:
            with self.subTest(fmt=fmt):
                image = Image.new('RGB', (40,30), (200,30,20))
                buf=io.BytesIO(); image.save(buf, format=fmt)
                code,meta,_=self.decode(buf.getvalue(),fmt.lower(),'inspect')
                self.assertEqual(code,0)
                self.assertEqual(json.loads(meta)['width'],40)
                code,_,preview=self.decode(buf.getvalue(),fmt.lower())
                self.assertEqual(code,0)
                with Image.open(io.BytesIO(preview)) as p:
                    self.assertEqual(p.format,'PNG'); self.assertEqual(p.size,(40,30))
                    self.assertFalse(p.getexif())
                self.assertEqual(self.decode(buf.getvalue(),fmt.lower(),pixels=100)[0],22)
                self.assertEqual(self.decode(buf.getvalue(), 'svg')[0],15)

    def test_svg(self):
        good=b'<svg xmlns="http://www.w3.org/2000/svg" width="40" height="30"><rect width="40" height="30" fill="red"/></svg>'
        code,_,preview=self.decode(good,'svg')
        self.assertEqual(code,0)
        with Image.open(io.BytesIO(preview)) as p:
            self.assertEqual(p.size,(40,30))
        attacks=[
            '<script>alert(1)</script>',
            '<rect onload="alert(1)"/>',
            '<image href="file:///etc/passwd"/>',
            '<image href="http://169.254.169.254/latest/meta-data/"/>',
            '<foreignObject><div>HTML</div></foreignObject>',
            '<style>@import "https://example.com";</style>',
            '<use href="#loop" id="loop"/>',
            '<rect fill="url(file:///etc/passwd)"/>',
            '<rect style="fill:red"/>',
            '<animate attributeName="href"/>',
        ]
        for attack in attacks:
            with self.subTest(attack=attack):
                data=('<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10">'+attack+'</svg>').encode()
                self.assertEqual(self.decode(data,'svg','inspect')[0],15)
        dtd=b'<!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]><svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><text>&x;</text></svg>'
        self.assertEqual(self.decode(dtd,'svg','inspect')[0],15)

    def test_local_path_use(self):
        for href in ('href', 'xlink:href'):
            doc = ('<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="20" height="10">'
                   '<use transform="translate(10 0)" '+href+'="#tile"/>'
                   '<path id="tile" fill="red" d="M0 0h8v8H0z"/></svg>').encode()
            code, _, preview = self.decode(doc, 'svg')
            self.assertEqual(code, 0)
            with Image.open(io.BytesIO(preview)) as image:
                self.assertEqual(image.convert('RGBA').getpixel((14,4)), (255,0,0,255))

    def test_multipage_and_spoofing(self):
        a=Image.new('RGB',(20,10),'red'); b=Image.new('RGB',(100,100),'blue')
        buffer=io.BytesIO(); a.save(buffer,format='TIFF',save_all=True,append_images=[b])
        code,_,preview=self.decode(buffer.getvalue(),'tiff')
        self.assertEqual(code,0)
        with Image.open(io.BytesIO(preview)) as p:
            self.assertEqual(p.size,(20,10))
        for fmt in ['raw','nef','dng','cr2']:
            self.assertEqual(self.decode(buffer.getvalue(),fmt,'inspect')[0],15)
            self.assertEqual(self.decode(b'not an image',fmt,'inspect')[0],15)

if __name__=='__main__':
    unittest.main()
