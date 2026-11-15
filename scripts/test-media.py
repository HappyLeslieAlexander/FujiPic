#!/usr/bin/env python3
"""Integration test against disposable compose.media-test.yml (stdlib only).

Usage: python3 scripts/test-media.py /path/to/generated-and-camera-fixtures
"""
import hashlib
import http.cookiejar
import json
from pathlib import Path
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE='http://127.0.0.1:18093'
jar=http.cookiejar.CookieJar()
client=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
def request(path, data=None, headers=None, method=None):
    req=urllib.request.Request(path if path.startswith('http') else BASE+path,data=data,headers=headers or {},method=method)
    try:
        response=client.open(req,timeout=110)
    except urllib.error.HTTPError as error:
        response=error
    with response:
        return response.status,response.headers,response.read()
def payload(path):
    status,_,body=request(path)
    assert status==200,(status,body)
    return json.loads(body)
status,_,body=request('/login')
csrf=re.search(rb'name="csrf" value="([^"]+)"',body).group(1).decode()
status,_,body=request('/login',urllib.parse.urlencode({'csrf':csrf,'email':'admin@example.test','password':'local-media-test-password'}).encode(),{'Content-Type':'application/x-www-form-urlencoded'})
assert status==200
csrf=re.search(rb'name="csrf" value="([^"]+)"',body).group(1).decode()
before=payload('/api/v1/me')['used_bytes']
root=Path(sys.argv[1])
files=[root/name for name in ['test.avif','test.bmp','test.tif','test.tiff','test.svg','test.dng','test.raw','RAW_CANON_40D_SRAW_V103.CR2','iss030e122639.NEF']]
uploaded={}
for file in files:
    data=file.read_bytes()
    status,_,body=request('/api/v1/upload',data,{'Content-Type':'application/octet-stream','X-Filename':file.name,'X-CSRF-Token':csrf})
    assert status in (200,201),(file.name,status,body)
    image=json.loads(body)
    uploaded[image['id']]=image
    assert image['processing_state'] in ('pending','processing','ready')
    status,headers,original=request(image['original_url'])
    assert status==200 and hashlib.sha256(original).digest()==hashlib.sha256(data).digest()
    assert headers['Content-Disposition']=='attachment' and headers['Content-Type']=='application/octet-stream'
    assert not headers.get('Set-Cookie') and not headers.get('Content-Language')
    assert headers['CDN-Cache-Control']=='public, max-age=300, must-revalidate'
    print('Original validated:',file.name,image['mime'])
for name,data in [('unsafe.svg',(root/'unsafe.svg').read_bytes()),('fake.nef',b'not raw'),('fake.avif',b'not avif')]:
    status,_,body=request('/api/v1/upload',data,{'Content-Type':'application/octet-stream','X-Filename':name,'X-CSRF-Token':csrf})
    assert status==415,(name,status,body)
deadline=time.monotonic()+180
while uploaded and time.monotonic()<deadline:
    states={item['id']:item for item in payload('/api/v1/images?limit=100')['items']}
    if all(states[key]['processing_state']=='ready' for key in uploaded):
        break
    assert not any(states[key]['processing_state']=='failed' for key in uploaded),states
    time.sleep(2)
else:
    raise AssertionError('preview deadline exceeded')
expected=before
urls=[]
for key,image in uploaded.items():
    item=states[key]
    status,headers,preview=request(item['preview_url'])
    assert status==200 and preview.startswith(b'\x89PNG\r\n\x1a\n')
    assert headers['Content-Type']=='image/png' and headers['Content-Disposition']=='inline'
    assert not headers.get('Set-Cookie')
    expected+=image['size']+len(preview)
    status,h,part=request(item['preview_url'],headers={'Range':'bytes=0-7'})
    assert status==206 and part==preview[:8]
    urls.extend([item['original_url'],item['preview_url']])
assert payload('/api/v1/me')['used_bytes']==expected
for key in uploaded:
    status,_,_=request('/api/v1/images/'+key,headers={'X-CSRF-Token':csrf},method='DELETE')
    assert status==204
for url in urls:
    status,headers,_=request(url)
    assert status==404 and headers['Cache-Control']=='no-store'
assert payload('/api/v1/me')['used_bytes']==before
print('PASS: all formats, async PNG previews, original hashes, attachment security, ranges, exact quota accounting, unsafe input rejection and deletion')
