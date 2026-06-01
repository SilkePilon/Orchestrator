#!/usr/bin/env python3
"""Regenerate flatpak-go-sources.json from go.sum using the module cache proxy format."""

import asyncio
import hashlib
import json
import re
import sys
import os

import aiohttp

PROXY = "https://proxy.golang.org"

def to_proxy_path(module_path: str) -> str:
    """Convert a Go module path to the proxy URL encoding (uppercase -> !lowercase)."""
    return re.sub(r'[A-Z]', lambda m: '!' + m.group(0).lower(), module_path)

def parse_gosum(path: str):
    """Return set of (module_path, version) tuples from go.sum."""
    modules = {}
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            parts = line.split()
            if len(parts) < 3:
                continue
            mod_path = parts[0]
            ver_field = parts[1]
            if ver_field.endswith('/go.mod'):
                ver = ver_field[:-len('/go.mod')]
                modules.setdefault((mod_path, ver), set()).add('mod')
            else:
                ver = ver_field
                modules.setdefault((mod_path, ver), set()).add('zip')
    return modules

async def fetch_sha256(session: aiohttp.ClientSession, url: str) -> str | None:
    try:
        async with session.get(url, timeout=aiohttp.ClientTimeout(total=60)) as resp:
            if resp.status == 200:
                data = await resp.read()
                return hashlib.sha256(data).hexdigest()
            return None
    except Exception as e:
        print(f"  WARN: {url}: {e}", file=sys.stderr)
        return None

async def main():
    gosum_path = sys.argv[1] if len(sys.argv) > 1 else 'go.sum'
    output_path = sys.argv[2] if len(sys.argv) > 2 else 'flatpak-go-sources.json'

    print(f"Parsing {gosum_path}...", file=sys.stderr)
    modules = parse_gosum(gosum_path)
    print(f"Found {len(modules)} module versions", file=sys.stderr)

    sources = []
    connector = aiohttp.TCPConnector(limit=20)
    async with aiohttp.ClientSession(connector=connector) as session:
        tasks = []
        entries = []

        for (mod_path, ver), file_types in sorted(modules.items()):
            proxy_path = to_proxy_path(mod_path)
            dest = f"go/pkg/mod/cache/download/{proxy_path}/@v"
            base_url = f"{PROXY}/{proxy_path}/@v/{ver}"

            # Always fetch .info and .mod; only fetch .zip if go.sum has zip entry
            exts = ['.info', '.mod']
            if 'zip' in file_types:
                exts.append('.zip')

            for ext in exts:
                url = f"{base_url}{ext}"
                entries.append((url, dest))
                tasks.append(fetch_sha256(session, url))

        print(f"Fetching {len(tasks)} files...", file=sys.stderr)
        results = await asyncio.gather(*tasks)

    ok = 0
    skip = 0
    for (url, dest), sha256 in zip(entries, results):
        if sha256 is None:
            print(f"  SKIP (not found): {url}", file=sys.stderr)
            skip += 1
            continue
        sources.append({
            "type": "file",
            "url": url,
            "sha256": sha256,
            "dest": dest,
        })
        ok += 1

    print(f"Generated {ok} entries ({skip} skipped)", file=sys.stderr)

    with open(output_path, 'w') as f:
        json.dump(sources, f, indent=4)
        f.write('\n')

    print(f"Written to {output_path}", file=sys.stderr)

asyncio.run(main())
