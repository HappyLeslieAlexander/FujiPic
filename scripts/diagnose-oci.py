#!/usr/bin/env python3
"""Read-only, independent SigV4 check against an OCI S3 path-style endpoint.

Uses only Python's standard library; does not use FujiPic, its database,
STORAGE_ENCRYPTION_KEY, or the AWS SDK. Credentials are hidden prompts and
never written to disk or printed. GETs a random nonexistent object, no writes.
"""

import datetime
import getpass
import hashlib
import hmac
import re
import secrets
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET


def authorization(host, path, region, access, secret, timestamp):
    date = timestamp[:8]
    payload = hashlib.sha256(b"").hexdigest()
    headers = f"host:{host}\nx-amz-content-sha256:{payload}\nx-amz-date:{timestamp}\n"
    names = "host;x-amz-content-sha256;x-amz-date"
    canonical = f"GET\n{path}\n\n{headers}\n{names}\n{payload}"
    scope = f"{date}/{region}/s3/aws4_request"
    to_sign = f"AWS4-HMAC-SHA256\n{timestamp}\n{scope}\n{hashlib.sha256(canonical.encode()).hexdigest()}"
    key = ("AWS4" + secret).encode()
    for value in (date, region, "s3", "aws4_request"):
        key = hmac.new(key, value.encode(), hashlib.sha256).digest()
    signature = hmac.new(key, to_sign.encode(), hashlib.sha256).hexdigest()
    return {
        "Authorization": f"AWS4-HMAC-SHA256 Credential={access}/{scope}, SignedHeaders={names}, Signature={signature}",
        "X-Amz-Date": timestamp,
        "X-Amz-Content-Sha256": payload,
    }


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def main():
    endpoint = input("OCI S3 endpoint (https://...): ").strip().rstrip("/")
    region = input("Region (e.g. af-johannesburg-1): ").strip()
    bucket = input("Bucket name: ").strip()
    parsed = urllib.parse.urlsplit(endpoint)
    match = re.fullmatch(r"[a-z0-9-]+\.compat\.objectstorage\.([a-z0-9-]+)\.(?:oraclecloud\.com|oci\.customer-oci\.com)", parsed.netloc)
    if parsed.scheme != "https" or parsed.path or parsed.query or parsed.fragment or not match:
        raise SystemExit("Use a standard OCI HTTPS path-style endpoint, without a bucket path.")
    if region != match[1]:
        raise SystemExit("Endpoint region and signing region do not match.")
    if not bucket or "/" in bucket:
        raise SystemExit("Invalid bucket name.")
    access = getpass.getpass("Access Key (hidden): ").strip()
    secret = getpass.getpass("Secret Key (hidden): ").strip()
    if not access or not secret:
        raise SystemExit("Both keys are required.")
    path = "/" + urllib.parse.quote(bucket, safe="") + "/.fujipic-diagnostic/" + secrets.token_hex(16)
    timestamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    request = urllib.request.Request(endpoint + path, headers=authorization(parsed.netloc, path, region, access, secret, timestamp))
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(request, timeout=30) as response:
            print("HTTP", response.status, "(unexpected existing diagnostic object; body not displayed)")
    except urllib.error.HTTPError as error:
        print("HTTP", error.code)
        print("Request ID:", error.headers.get("opc-request-id", error.headers.get("x-amz-request-id", "")))
        try:
            root = ET.fromstring(error.read(65536))
            fields = {node.tag.rsplit("}", 1)[-1]: node.text or "" for node in root}
            for name in ("Code", "Message"):
                # Some server errors echo the access ID; redact both credentials.
                value = fields.get(name, "").replace(access, "[redacted]").replace(secret, "[redacted]")
                print(name + ":", value)
            if fields.get("Code") == "NoSuchKey":
                print("Expected missing object: authentication/read lookup succeeded. Write/delete permissions are not tested.")
        except ET.ParseError:
            print("Non-XML error response; body not displayed.")
    except urllib.error.URLError as error:
        print("Connection/TLS error:", error.reason)


if __name__ == "__main__":
    main()
