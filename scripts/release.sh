#!/usr/bin/env bash
set -euo pipefail

version="${VERSION:-1.0.0-rc.1}"
commit="${COMMIT:-$(git rev-parse --verify HEAD 2>/dev/null || printf unknown)}"
source_date_epoch="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || date +%s)}"
build_date="${BUILD_DATE:-$(date -u -d "@${source_date_epoch}" +%Y-%m-%dT%H:%M:%SZ)}"
dist_dir="${DIST_DIR:-dist}"

case "$version" in
  *[!A-Za-z0-9._+-]*) echo "invalid VERSION: $version" >&2; exit 64 ;;
esac
case "$source_date_epoch" in
  ''|*[!0-9]*) echo "SOURCE_DATE_EPOCH must be an integer" >&2; exit 64 ;;
esac

rm -rf "$dist_dir"
mkdir -p "$dist_dir"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

build_one() {
  local os="$1" arch="$2"
  local base="aninode_${version}_${os}_${arch}"
  local root="$work/$base"
  mkdir -p "$root/config.example"

  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X main.version=${version} -X main.commit=${commit} -X main.buildDate=${build_date}" \
      -o "$root/aninode" ./cmd/aninode

  cp LICENSE README.md README_CN.md compose.yaml .env.example "$root/"
  cp -R config.example/. "$root/config.example/"
  find "$root" -exec touch -h -d "@${source_date_epoch}" {} +
  chmod 0755 "$root/aninode"

  tar --sort=name --owner=0 --group=0 --numeric-owner \
    --mtime="@${source_date_epoch}" -C "$work" -cf - "$base" \
    | gzip -n > "$dist_dir/$base.tar.gz"
}

build_one linux amd64
build_one linux arm64

(
  cd "$dist_dir"
  sha256sum ./*.tar.gz | LC_ALL=C sort -k2 > SHA256SUMS
)

# SPDX 2.3 artifact SBOM. The module has no third-party Go dependencies;
# this records release files plus their SHA-256 digests with standard SPDX IDs.
python3 - "$dist_dir" "$version" "$commit" "$build_date" <<'PY'
import hashlib, json, os, pathlib, re, sys

dist, version, commit, build_date = sys.argv[1:]
files = sorted(pathlib.Path(dist).glob("*.tar.gz"))
def sid(name):
    return "SPDXRef-File-" + re.sub(r"[^A-Za-z0-9.-]", "-", name)
doc = {
    "spdxVersion": "SPDX-2.3",
    "dataLicense": "CC0-1.0",
    "SPDXID": "SPDXRef-DOCUMENT",
    "name": f"aninode-{version}-release-artifacts",
    "documentNamespace": f"https://github.com/mistarille224/aninode/releases/{version}/{commit}",
    "creationInfo": {"created": build_date, "creators": ["Tool: scripts/release.sh"]},
    "packages": [{
        "name": "aninode",
        "SPDXID": "SPDXRef-Package-aninode",
        "versionInfo": version,
        "downloadLocation": "NOASSERTION",
        "filesAnalyzed": True,
        "licenseConcluded": "Apache-2.0",
        "licenseDeclared": "Apache-2.0",
        "copyrightText": "NOASSERTION",
    }],
    "files": [],
    "relationships": [],
}
for p in files:
    digest = hashlib.sha256(p.read_bytes()).hexdigest()
    fid = sid(p.name)
    doc["files"].append({
        "fileName": p.name,
        "SPDXID": fid,
        "checksums": [{"algorithm": "SHA256", "checksumValue": digest}],
        "licenseConcluded": "NOASSERTION",
        "copyrightText": "NOASSERTION",
    })
    doc["relationships"].append({
        "spdxElementId": "SPDXRef-Package-aninode",
        "relationshipType": "CONTAINS",
        "relatedSpdxElement": fid,
    })
path = pathlib.Path(dist) / f"aninode_{version}_artifacts.spdx.json"
path.write_text(json.dumps(doc, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY

(
  cd "$dist_dir"
  sha256sum ./*.spdx.json >> SHA256SUMS
  LC_ALL=C sort -k2 -o SHA256SUMS SHA256SUMS
)

printf 'release artifacts written to %s\n' "$dist_dir"
