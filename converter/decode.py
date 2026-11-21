"""One untrusted file per process. No URL fetches, shell commands or delegates."""
import io
import json
import math
import os
import re
import resource
import sys
import struct
import warnings
from xml.etree import ElementTree as XML

# Set limits before importing native decoders. The container adds an aggregate
# memory/PID/CPU bound, and the parent kills the entire process group on timeout.
resource.setrlimit(resource.RLIMIT_CPU, (60, 60))
resource.setrlimit(resource.RLIMIT_FSIZE, (16 << 20, 16 << 20))
resource.setrlimit(resource.RLIMIT_NOFILE, (64, 64))
resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
if sys.platform == 'linux':
    resource.setrlimit(resource.RLIMIT_AS, (1536 << 20, 1536 << 20))

from PIL import Image, ImageOps, ImageCms
from defusedxml import ElementTree

class Dimensions(Exception):
    pass

# Stable subprocess results; parser messages and file contents never cross
# the socket. Unknown/malformed input retains the generic rejection.
class UnsupportedFeature(ValueError):
    pass

class SvgComplexity(ValueError):
    pass

class SvgSize(ValueError):
    pass

def dimensions(width, height, pixels, dimension):
    if width <= 0 or height <= 0 or width > dimension or height > dimension or width * height > pixels:
        raise Dimensions()

SVG_NS = 'http://www.w3.org/2000/svg'
ELEMENTS = set('svg g defs symbol use title desc path rect circle ellipse line polyline polygon text tspan linearGradient radialGradient stop clipPath'.split())
ATTRS = set('id x y x1 y1 x2 y2 cx cy r rx ry width height viewBox preserveAspectRatio d points transform fill fill-opacity fill-rule stroke stroke-width stroke-opacity stroke-linecap stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset opacity clip-path clip-rule gradientUnits gradientTransform spreadMethod offset stop-color stop-opacity font-family font-size font-weight font-style text-anchor dominant-baseline dx dy rotate'.split())

def static_paint_style(value):
    # Exporters commonly write literal paints as style instead of attributes.
    # Translate only this closed grammar; never pass CSS through to CairoSVG.
    attrs = {}
    for declaration in value.split(';'):
        if not declaration.strip():
            continue
        name, _, paint = declaration.partition(':')
        name, paint = name.strip().lower(), paint.strip().lower()
        if name not in ('fill', 'stroke') or not re.fullmatch(r'none|#[0-9a-f]{3}(?:[0-9a-f]{3})?', paint):
            raise UnsupportedFeature('unsupported inline SVG style')
        attrs[name] = paint
    return attrs

def svg_document(path, pixels, dimension):
    # A deliberately restricted SVG profile. Reject unsupported active content
    # rather than silently stripping elements and pretending fidelity is intact.
    with open(path, 'rb') as source:
        data = source.read((4 << 20) + 1)
    if len(data) > 4 << 20:
        raise SvgSize()
    root = ElementTree.fromstring(data, forbid_dtd=True, forbid_entities=True, forbid_external=True)
    if root.tag != '{'+SVG_NS+'}svg':
        raise ValueError('not SVG')
    count = 0
    stack = [(root, 0)]
    ids = {}
    uses = []
    normalized = False
    for element in root.iter():
        identifier = element.attrib.get('id')
        if identifier:
            if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_.-]{0,127}', identifier) or identifier in ids:
                raise ValueError('invalid ID')
            ids[identifier] = element
    while stack:
        element, depth = stack.pop()
        count += 1
        if count > 10000 or depth > 32:
            raise SvgComplexity()
        if not element.tag.startswith('{'+SVG_NS+'}') or element.tag.split('}', 1)[1] not in ELEMENTS:
            raise UnsupportedFeature('unsupported SVG element')
        is_use = element.tag == '{'+SVG_NS+'}use'
        if is_use:
            uses.append(element)
        if element is root and element.get('version') in ('1.0', '1.1', '2.0'):
            del element.attrib['version']
            normalized = True
        if 'style' in element.attrib:
            value = element.attrib.pop('style')
            if len(value) > 65536:
                raise SvgComplexity()
            element.attrib.update(static_paint_style(value))
            normalized = True
        for name, value in element.attrib.items():
            if len(value) > 65536:
                raise SvgComplexity()
            if name in ('href', '{http://www.w3.org/1999/xlink}href'):
                if is_use:
                    continue  # Resolved locally only after validating every element.
                raise UnsupportedFeature('SVG references disabled')
            if name not in ATTRS or any(c in value for c in ['\\', '@', '<', '>']):
                raise UnsupportedFeature('unsupported SVG attribute')
            if 'url' in value.lower():
                match = re.fullmatch(r'url\(#([A-Za-z_][A-Za-z0-9_.-]{0,127})\)', value)
                if name not in ('fill', 'stroke', 'clip-path') or not match or match.group(1) not in ids:
                    raise UnsupportedFeature('unsupported SVG resource')
        stack.extend((child, depth+1) for child in element)
    # Flatten a small, static subset of <use>. The renderer never follows the
    # reference; reject groups, nested references and unmodelled positioning.
    if uses:
        if len(uses) > 256 or count + len(uses) > 10000:
            raise SvgComplexity()
        expanded = 0
        for element in uses:
            refs = [element.attrib.pop(k) for k in ('href', '{http://www.w3.org/1999/xlink}href') if k in element.attrib]
            if len(refs) != 1 or not refs[0].startswith('#') or len(element) or (element.text or '').strip():
                raise UnsupportedFeature('unsupported SVG use')
            target = ids.get(refs[0][1:])
            if (target is None or target.tag != '{'+SVG_NS+'}path' or len(target)
                    or (target.text or '').strip() or not set(element.attrib) <= {'transform', 'fill', 'stroke', 'opacity'}
                    or any('url' in v.lower() for v in (*element.attrib.values(), *target.attrib.values()))):
                raise UnsupportedFeature('unsupported SVG use target or attributes')
            expanded += sum(len(v) for v in target.attrib.values())
            if expanded > 1 << 20:
                raise SvgComplexity()
            element.tag = '{'+SVG_NS+'}g'
            XML.SubElement(element, target.tag, {k: v for k, v in target.attrib.items() if k != 'id'})
    if uses or normalized:
        data = XML.tostring(root, encoding='utf-8')
    def length(value):
        if value is None:
            return None
        if not re.fullmatch(r'[0-9]+(?:\.[0-9]+)?(?:px)?', value):
            raise UnsupportedFeature('unsupported SVG dimension')
        return float(value.removesuffix('px'))
    width, height = length(root.get('width')), length(root.get('height'))
    if width is None or height is None:
        box = re.split(r'[\s,]+', root.get('viewBox', '').strip())
        if len(box) != 4:
            raise ValueError('SVG needs dimensions')
        values = [float(v) for v in box]
        if not all(math.isfinite(v) for v in values):
            raise ValueError('invalid viewBox')
        width = width if width is not None else values[2]
        height = height if height is not None else values[3]
    dimensions(width, height, pixels, dimension)
    return data, math.ceil(width), math.ceil(height)

def open_raw(path, fmt, pixels, dimension):
    import rawpy
    # Validate a raw container hint as well as asking LibRaw to actually open it.
    with open(path, 'rb') as source:
        head = source.read(16)
    if fmt == 'cr2' and head[8:12] != b'CR\x02\x00':
        raise ValueError('not CR2')
    if fmt in ('nef', 'dng'):
        if head[:4] not in (b'II*\x00', b'MM\x00*'):
            raise ValueError('not TIFF raw')
        # Only inspect the bounded first IFD. A generic TIFF image loader often
        # cannot open sensor data; it must not gate valid NEF/DNG containers.
        with open(path, 'rb') as file:
            order = '<' if head[:2] == b'II' else '>'
            offset = struct.unpack(order+'I', head[4:8])[0]
            if offset < 8 or offset > os.fstat(file.fileno()).st_size-2:
                raise ValueError('invalid IFD')
            file.seek(offset)
            count = struct.unpack(order+'H', file.read(2))[0]
            if count > 4096:
                raise ValueError('IFD too large')
            entries = file.read(count*12)
            if len(entries) != count*12:
                raise ValueError('truncated IFD')
            found = False
            for index in range(count):
                entry = entries[index*12:(index+1)*12]
                tag, kind, number = struct.unpack(order+'HHI', entry[:8])
                if fmt == 'dng' and tag == 50706 and kind == 1 and number == 4:
                    found = entry[8] == 1
                if fmt == 'nef' and tag == 271 and kind == 2 and 5 <= number <= 128:
                    file.seek(struct.unpack(order+'I', entry[8:])[0])
                    found = file.read(number).upper().startswith(b'NIKON')
            if not found:
                raise ValueError('raw format mismatch')
    # LibRaw also reports "unsupported" for unrelated bytes; it cannot
    # reliably distinguish those from a valid but unsupported camera model.
    raw = rawpy.imread(path)
    try:
        dimensions(raw.sizes.raw_width, raw.sizes.raw_height, pixels, dimension)
        dimensions(raw.sizes.width, raw.sizes.height, pixels, dimension)
    except Exception:
        raw.close()
        raise
    return raw

def decode(op, path, fmt, output, pixels, dimension):
    Image.MAX_IMAGE_PIXELS = pixels
    warnings.simplefilter('error', Image.DecompressionBombWarning)
    if fmt == 'svg':
        data, width, height = svg_document(path, pixels, dimension)
        if op == 'inspect':
            return {'format': fmt, 'width': width, 'height': height}
        import cairosvg
        scale = min(1, 2048 / max(width, height))
        # Custom fetcher denies even file/data URIs should validation miss one.
        def no_fetch(*args, **kwargs):
            raise ValueError('resource access disabled')
        tree = cairosvg.parser.Tree(bytestring=data, url_fetcher=no_fetch, unsafe=False)
        buffer = io.BytesIO()
        cairosvg.surface.PNGSurface(tree, buffer, 96, output_width=max(1, int(width*scale)), output_height=max(1, int(height*scale))).finish()
        image = Image.open(io.BytesIO(buffer.getvalue()))
    elif fmt in ('raw', 'nef', 'dng', 'cr2'):
        with open_raw(path, fmt, pixels, dimension) as raw:
            width, height = raw.sizes.width, raw.sizes.height
            if op == 'inspect':
                return {'format': fmt, 'width': width, 'height': height}
            # Full decoding is intentional: no silently low-resolution embedded
            # thumbnail presented as a full sensor rendering.
            rgb = raw.postprocess(use_camera_wb=True, no_auto_bright=True, output_bps=8, output_color=__import__('rawpy').ColorSpace.sRGB)
            image = Image.fromarray(rgb)
    else:
        image = Image.open(path)
        if image.format != {'avif': 'AVIF', 'bmp': 'BMP', 'tiff': 'TIFF'}[fmt]:
            raise ValueError('format mismatch')
        width, height = image.size
        dimensions(width, height, pixels, dimension)
        if fmt == 'tiff' and (50706 in image.tag_v2 or image.tag_v2.get(262) in (32803, 34892)):
            raise ValueError('raw must use raw decoder')
        if fmt == 'avif' and getattr(image, 'n_frames', 1) != 1:
            raise UnsupportedFeature('animated AVIF not supported')
        if op == 'inspect':
            image.verify()
            return {'format': fmt, 'width': width, 'height': height}
        # TIFF intentionally renders only the first page; other pages remain in
        # the original download and cannot multiply conversion work.
        image.seek(0)
    image = ImageOps.exif_transpose(image)
    image.thumbnail((2048, 2048), Image.Resampling.LANCZOS)
    profile = image.info.get('icc_profile')
    alpha = image.getchannel('A') if 'A' in image.getbands() else None
    if profile:
        if len(profile) > 1 << 20:
            raise ValueError('ICC too large')
        image = ImageCms.profileToProfile(image, ImageCms.ImageCmsProfile(io.BytesIO(profile)), ImageCms.createProfile('sRGB'), outputMode='RGB')
        if alpha is not None:
            image.putalpha(alpha)
    else:
        image = image.convert('RGBA' if alpha is not None else 'RGB')
    # Drop EXIF/GPS/text/profile payloads; only freshly encoded pixels leave.
    image.info.clear()
    image.save(output, format='PNG', optimize=False)
    return {'format': fmt, 'width': width, 'height': height}

if __name__ == '__main__':
    try:
        op, path, fmt, output, pixels, dimension = sys.argv[1:]
        result = decode(op, path, fmt, output, int(pixels), int(dimension))
        print(json.dumps(result))
    except (Dimensions, Image.DecompressionBombError, Image.DecompressionBombWarning):
        sys.exit(22)
    except UnsupportedFeature:
        sys.exit(16)
    except SvgComplexity:
        sys.exit(17)
    except SvgSize:
        sys.exit(18)
    except Exception:
        # Never expose parser details, metadata or filesystem paths to clients.
        sys.exit(15)
