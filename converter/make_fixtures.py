"""Generate tiny deterministic, original fixtures (including an uncompressed DNG)."""
from pathlib import Path
import struct
import sys
from PIL import Image

root=Path(sys.argv[1]); root.mkdir(parents=True, exist_ok=True)
image=Image.new('RGB',(40,30),(200,40,20))
for name,fmt in [('test.avif','AVIF'),('test.bmp','BMP'),('test.tif','TIFF'),('test.tiff','TIFF')]:
    image.save(root/name,format=fmt)
(root/'test.svg').write_text('<svg xmlns="http://www.w3.org/2000/svg" width="40" height="30"><rect width="40" height="30" fill="#c82814"/></svg>')
(root/'unsafe.svg').write_text('<svg xmlns="http://www.w3.org/2000/svg" width="40" height="30" onload="alert(1)"><script>alert(1)</script></svg>')

# Minimal CFA DNG, no downloaded/private photographs needed for this fixture.
entries={}
def tag(number,kind,values):
    if kind==2:
        data=values.encode()+b'\0'; count=len(data)
    elif kind==1:
        data=bytes(values); count=len(values)
    elif kind in (3,4):
        data=struct.pack('<'+('H' if kind==3 else 'I')*len(values),*values); count=len(values)
    else:
        data=b''.join(struct.pack('<ii' if kind==10 else '<II',a,b) for a,b in values); count=len(values)
    entries[number]=(kind,count,data)
tag(256,4,[64]); tag(257,4,[64]); tag(258,3,[16]); tag(259,3,[1]); tag(262,3,[32803])
tag(271,2,'FujiPic'); tag(272,2,'Test CFA'); tag(273,4,[0]); tag(277,3,[1]); tag(278,4,[64]); tag(279,4,[8192]); tag(284,3,[1])
tag(33421,3,[2,2]); tag(33422,1,[0,1,1,2]); tag(50706,1,[1,4,0,0]); tag(50707,1,[1,1,0,0]); tag(50708,2,'FujiPic Test CFA')
tag(50710,1,[0,1,2]); tag(50711,3,[1]); tag(50714,5,[(0,1)]); tag(50717,4,[65535]); tag(50719,4,[0,0]); tag(50720,4,[64,64])
tag(50721,10,[(1 if i in (0,4,8) else 0,1) for i in range(9)])
tag(50728,5,[(1,1)]*3); tag(50778,3,[21])
offset=8+2+12*len(entries)+4
extra=bytearray(); records=[]
for number,(kind,count,data) in sorted(entries.items()):
    if len(data)<=4:
        field=data.ljust(4,b'\0')
    else:
        field=struct.pack('<I',offset+len(extra)); extra.extend(data)
        if len(extra)%2: extra.append(0)
    records.append((number,kind,count,field))
pixel_offset=offset+len(extra)
records=[(n,k,c,struct.pack('<I',pixel_offset) if n==273 else f) for n,k,c,f in records]
data=b'II*\0'+struct.pack('<I',8)+struct.pack('<H',len(records))+b''.join(struct.pack('<HHI',n,k,c)+f for n,k,c,f in records)+b'\0'*4+extra
data+=b''.join(struct.pack('<H',20000+(x+y)*100) for y in range(64) for x in range(64))
(root/'test.dng').write_bytes(data)
(root/'test.raw').write_bytes(data[:-2]+struct.pack('<H',12345))
print('Generated AVIF, BMP, TIFF/TIF, SVG, DNG and RAW fixtures')
