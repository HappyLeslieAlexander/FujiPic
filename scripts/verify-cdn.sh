#!/usr/bin/env bash
# Verify the CDN -> FujiPic -> private S3 chain.
#
# Usage:
#   FUJIPIC_TOKEN=... ./scripts/verify-cdn.sh https://pic.example.com https://img.example.com [local-image]
#
# Requires curl and python3. The token needs upload,read,delete scopes.
set -euo pipefail

PIC_BASE="${1:?usage: FUJIPIC_TOKEN=... verify-cdn.sh PIC_BASE IMG_BASE [FILE]}"
IMG_BASE="${2:?usage: FUJIPIC_TOKEN=... verify-cdn.sh PIC_BASE IMG_BASE [FILE]}"
TOKEN="${FUJIPIC_TOKEN:?set FUJIPIC_TOKEN}"
FILE="${3:-}"

PIC_BASE="${PIC_BASE%/}"
IMG_BASE="${IMG_BASE%/}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
umask 077
printf 'Authorization: Bearer %s\n' "$TOKEN" > "$tmpdir/auth-header"
unset TOKEN FUJIPIC_TOKEN

if [[ -z "$FILE" ]]; then
  # 1x1 PNG
  printf 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=' \
    | base64 -d > "$tmpdir/test.png" 2>/dev/null \
    || printf 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=' | base64 -D > "$tmpdir/test.png"
  FILE="$tmpdir/test.png"
fi

pass=0
fail=0
check() {
  local name="$1" expected="$2" actual="$3"
  if [[ "$actual" == "$expected" ]]; then
    echo "PASS  $name ($actual)"
    pass=$((pass + 1))
  else
    echo "FAIL  $name: expected $expected, got $actual"
    fail=$((fail + 1))
  fi
}

echo "== 1. upload through CDN =="
status=$(curl -sS -o "$tmpdir/upload.json" -w '%{http_code}' \
  -X POST "$PIC_BASE/api/v1/upload" \
  -H "@$tmpdir/auth-header" \
  -F "file=@$FILE")
# Both statuses mean upload success, but only a fresh image belongs to this
# destructive test. Never delete an existing user image returned by deduplication.
case "$status" in
  200|201)
    check "upload status" "$status" "$status"
    ;;
  *)
    check "upload status" "200/201" "$status"
    echo "Upload failed. Server response:" >&2
    cat "$tmpdir/upload.json" >&2
    exit 1
    ;;
esac
if [[ "$status" == "200" ]] || python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get("deduplicated") else 1)' "$tmpdir/upload.json"; then
  echo "STOP  upload matched an existing image; preserved it. Repeat with a new, disposable image or a dedicated test account." >&2
  exit 1
fi
image_url=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["url"])' "$tmpdir/upload.json")
image_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$tmpdir/upload.json")
case "$image_url" in
  "$IMG_BASE"/*)
    ;;
  *)
    echo "FAIL  image URL does not use IMG_BASE ($IMG_BASE): $image_url" >&2
    exit 1
    ;;
esac
echo "      url: $image_url"

echo "== 2. first image request (expect MISS) =="
curl -sS -D "$tmpdir/h1" -o "$tmpdir/download1" "$image_url"
head -1 "$tmpdir/h1"
grep -iE '^(cache-control|cdn-cache-control|etag|content-type|content-length|accept-ranges|cf-cache-status|x-cache):' "$tmpdir/h1" || true
cmp -s "$FILE" "$tmpdir/download1" && check "downloaded bytes" "identical" "identical" || check "downloaded bytes" "identical" "different"

echo "== 3. second image request (expect HIT) =="
curl -sS -D "$tmpdir/h2" -o /dev/null "$image_url"
head -1 "$tmpdir/h2"
cache_state=$(grep -iE '^(cf-cache-status|x-cache):' "$tmpdir/h2" | head -1 || true)
echo "      cache header: ${cache_state:-not provided by this CDN}"

echo "== 4. HEAD / Range / 304 =="
head_status=$(curl -sS -I -o "$tmpdir/head" -w '%{http_code}' "$image_url")
check "HEAD status" "200" "$head_status"
etag=$(awk -F': ' 'tolower($1)=="etag"{print $2}' "$tmpdir/head" | tr -d '\r' | head -1)
range_status=$(curl -sS -o "$tmpdir/range" -w '%{http_code}' -H 'Range: bytes=0-9' "$image_url")
check "Range status" "206" "$range_status"
check "Range length" "10" "$(wc -c < "$tmpdir/range" | tr -d ' ')"
if [[ -n "$etag" ]]; then
  cond_status=$(curl -sS -o /dev/null -w '%{http_code}' -H "If-None-Match: $etag" "$image_url")
  check "If-None-Match status" "304" "$cond_status"
fi

echo "== 5. API responses are not cached =="
api_cc=$(curl -sS -D - -o /dev/null -H "@$tmpdir/auth-header" "$PIC_BASE/api/v1/me" | awk -F': ' 'tolower($1)=="cache-control"{print $2}' | tr -d '\r')
check "API Cache-Control" "no-store" "$api_cc"

echo "== 6. delete and verify origin 404 =="
del_status=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$PIC_BASE/api/v1/images/$image_id" -H "@$tmpdir/auth-header")
check "delete status" "204" "$del_status"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print("\n".join([d["url"]]+d.get("alternate_urls",[])))' "$tmpdir/upload.json" > "$tmpdir/urls"
while IFS= read -r target; do
  after_status="000"
  for attempt in {1..12}; do
    after_status=$(curl --max-time 15 -sS -o /dev/null -w '%{http_code}' "$target")
    [[ "$after_status" == "404" ]] && break
    sleep 5
  done
  check "image after delete ($target)" "404" "$after_status"
done < "$tmpdir/urls"

echo
echo "passed: $pass, failed: $fail"
[[ "$fail" -eq 0 ]]
