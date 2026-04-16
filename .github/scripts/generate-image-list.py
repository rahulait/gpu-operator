#!/usr/bin/env python3
# Copyright NVIDIA CORPORATION
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Generate a complete list of container images required to run the GPU Operator.

Parses the Helm chart's values.yaml (and the bundled NFD subchart) to produce a
plain-text file with one fully-qualified image reference per line.  The list is
suitable for pre-pulling images in air-gapped environments.

For components whose images are OS-specific (driver, nvidia-fs/GDS, gdrdrv/GDRCopy)
the script queries the container registry to enumerate all available OS-variant tags
for the configured version (e.g. 595.58.03-ubuntu22.04, 595.58.03-rhel9.4, …).
Pass --skip-registry to disable network calls and fall back to the single tag from
values.yaml.

Usage:
    python3 generate-image-list.py [OPTIONS]

Options:
    --values        PATH   Path to gpu-operator values.yaml
                           (default: deployments/gpu-operator/values.yaml)
    --chart         PATH   Path to gpu-operator Chart.yaml
                           (default: deployments/gpu-operator/Chart.yaml)
    --nfd-values    PATH   Path to the bundled NFD values.yaml
                           (default: deployments/gpu-operator/charts/node-feature-discovery/values.yaml)
    --nfd-chart     PATH   Path to the bundled NFD Chart.yaml
                           (default: deployments/gpu-operator/charts/node-feature-discovery/Chart.yaml)
    --output        PATH   Write image list to PATH instead of stdout
    --no-nfd               Exclude the NFD subchart images from the output
    --skip-registry        Skip registry tag lookups (use version from values.yaml as-is)
    --gpu-operator-version VERSION
                           Override the gpu-operator image version (e.g., v1.0.0)
"""

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

try:
    import yaml
except ImportError:
    print("Error: PyYAML is required. Install it with: pip install pyyaml", file=sys.stderr)
    sys.exit(1)


# ---------------------------------------------------------------------------
# YAML helpers
# ---------------------------------------------------------------------------

def _load_yaml(path: str) -> dict:
    with open(path) as fh:
        return yaml.safe_load(fh) or {}


def _build_ref(repository: str, image: str, version: str) -> str | None:
    """Return a fully-qualified image reference, or None if any part is missing."""
    repository = (repository or "").strip()
    image = (image or "").strip()
    version = (version or "").strip()
    if not repository or not image or not version:
        return None
    return f"{repository}/{image}:{version}"


# ---------------------------------------------------------------------------
# Registry API helpers (Docker Registry v2 / OCI Distribution spec)
# ---------------------------------------------------------------------------

def _registry_token(registry_host: str, namespace: str) -> str:
    """Obtain an anonymous Bearer token for pulling from nvcr.io.

    nvcr.io advertises:
        WWW-Authenticate: Bearer realm="https://nvcr.io/proxy_auth",scope=""
    A GET to that realm with the desired scope returns {"token": "…"}.
    """
    scope = f"repository:{namespace}:pull"
    url = f"https://{registry_host}/proxy_auth?scope={urllib.parse.quote(scope)}"
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as resp:
        data = json.loads(resp.read())
    token = data.get("token") or data.get("access_token")
    if not token:
        raise RuntimeError(f"No token returned from {url}: {data}")
    return token


def _fetch_all_tags(registry_host: str, namespace: str) -> list[str]:
    """Return every tag for registry_host/namespace using the v2 tags/list API.

    Handles RFC 5988-style Link header pagination automatically.
    """
    token = _registry_token(registry_host, namespace)
    tags: list[str] = []
    url = f"https://{registry_host}/v2/{namespace}/tags/list?n=1000"

    while url:
        req = urllib.request.Request(
            url,
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/json",
            },
        )
        with urllib.request.urlopen(req, timeout=15) as resp:
            body = json.loads(resp.read())
            tags.extend(body.get("tags") or [])
            # Follow Link: <url>; rel="next" pagination
            link_header = resp.headers.get("Link", "")
            next_url = _parse_link_next(link_header)
            if next_url:
                # Link URLs from nvcr.io are relative paths; make them absolute
                if next_url.startswith("/"):
                    next_url = f"https://{registry_host}{next_url}"
                url = next_url
            else:
                url = None

    return tags


def _parse_link_next(link_header: str) -> str | None:
    """Extract the URL from a `Link: <url>; rel="next"` header, if present."""
    for part in link_header.split(","):
        part = part.strip()
        m = re.match(r'<([^>]+)>.*rel=["\']?next["\']?', part)
        if m:
            return m.group(1)
    return None


def _os_variant_tags(
    registry_host: str,
    namespace: str,
    version: str,
    fallback_ref: str,
    skip_registry: bool,
) -> list[str]:
    """Return all OS-variant image refs for a given component version.

    Queries the registry for tags matching ``<version>-<os-suffix>``.
    Supply-chain artefact tags (*.sbom, *.sig, *.vex, sha256-*) are excluded.

    Falls back to [fallback_ref] when:
    - skip_registry is True, or
    - the registry is unreachable / returns no matching tags.
    """
    if skip_registry:
        return [fallback_ref] if fallback_ref else []

    try:
        all_tags = _fetch_all_tags(registry_host, namespace)
    except Exception as exc:  # noqa: BLE001
        print(f"  Warning: registry query failed ({exc}); using fallback tag.",
              file=sys.stderr)
        return [fallback_ref] if fallback_ref else []

    prefix = f"{version}-"
    # Exclude supply-chain artefact pseudo-tags
    _exclude = re.compile(r"\.(sbom|sig|vex)$|^sha256-")
    matched = [
        f"{registry_host}/{namespace}:{t}"
        for t in all_tags
        if t.startswith(prefix) and not _exclude.search(t)
    ]

    if not matched:
        print(f"  Warning: no tags found matching {version}-* in "
              f"{registry_host}/{namespace}; using fallback tag.",
              file=sys.stderr)
        return [fallback_ref] if fallback_ref else []

    return sorted(matched)


# ---------------------------------------------------------------------------
# GPU Operator component image extraction
# ---------------------------------------------------------------------------

def _extract_operator_images(
    values: dict,
    app_version: str,
    skip_registry: bool,
    operator_version: str | None = None,
) -> list[str]:
    """Return all image references from the GPU Operator values.yaml.

    For components whose images carry an OS-suffix in the tag
    (driver, nvidia-fs/GDS, gdrdrv/GDRCopy), the registry is queried to
    enumerate every available OS variant of the configured version.
    """
    images: set[str] = set()

    def add(repo: str, img: str, ver: str | None) -> None:
        """Add a single-tag image, substituting app_version when version is absent."""
        if ver == "":
            return  # empty string = user-supplied image not set; skip
        resolved_ver = ver if ver else app_version
        ref = _build_ref(repo, img, resolved_ver)
        if ref:
            images.add(ref)

    def add_os_variants(repo: str, img: str, ver: str) -> None:
        """Add all OS-variant tags for an image by querying the registry.

        The convention for OS-specific images is:
            <repository>/<image>:<version>-<os-tag>
        e.g., nvcr.io/nvidia/driver:595.58.03-ubuntu22.04
        """
        if not repo or not img or not ver:
            return
        fallback = _build_ref(repo, img, ver)  # tag as written in values.yaml (no OS suffix)
        # Extract the registry host from the repository URL
        parts = repo.split("/", 1)
        registry_host = parts[0]
        # namespace = everything after the host + "/" + image name
        namespace = f"{parts[1]}/{img}" if len(parts) > 1 else img
        for ref in _os_variant_tags(registry_host, namespace, ver, fallback, skip_registry):
            images.add(ref)

    # ------------------------------------------------------------------
    # Components whose version defaults to Chart.appVersion when unset
    # ------------------------------------------------------------------
    for key in ("operator", "validator", "nodeStatusExporter"):
        comp = values.get(key, {})
        # Use explicit version from component if specified, otherwise use override if provided
        version = comp.get("version") or operator_version
        add(comp.get("repository", ""), comp.get("image", ""), version)

    # ------------------------------------------------------------------
    # Components with explicit, pinned versions
    # ------------------------------------------------------------------

    # NVIDIA Driver – OS-specific image (e.g. 595.58.03-ubuntu22.04)
    driver = values.get("driver", {})
    add_os_variants(
        driver.get("repository", ""),
        driver.get("image", ""),
        (driver.get("version") or "").strip(),
    )

    # k8s-driver-manager sidecar
    dm = driver.get("manager", {})
    add(dm.get("repository", ""), dm.get("image", ""), dm.get("version", ""))

    # Container Toolkit
    tk = values.get("toolkit", {})
    add(tk.get("repository", ""), tk.get("image", ""), tk.get("version", ""))

    # Device Plugin
    dp = values.get("devicePlugin", {})
    add(dp.get("repository", ""), dp.get("image", ""), dp.get("version", ""))

    # Standalone DCGM hostengine (optional, disabled by default)
    dcgm = values.get("dcgm", {})
    add(dcgm.get("repository", ""), dcgm.get("image", ""), dcgm.get("version", ""))

    # DCGM Exporter
    de = values.get("dcgmExporter", {})
    add(de.get("repository", ""), de.get("image", ""), de.get("version", ""))

    # GPU Feature Discovery (shares the device-plugin image)
    gfd = values.get("gfd", {})
    add(gfd.get("repository", ""), gfd.get("image", ""), gfd.get("version", ""))

    # MIG Manager
    mm = values.get("migManager", {})
    add(mm.get("repository", ""), mm.get("image", ""), mm.get("version", ""))

    # GPUDirect Storage – OS-specific image (e.g. 2.27.3-ubuntu22.04)
    gds = values.get("gds", {})
    add_os_variants(
        gds.get("repository", ""),
        gds.get("image", ""),
        (gds.get("version") or "").strip(),
    )

    # GDRCopy – OS-specific image (e.g. v2.5.2-ubuntu22.04)
    gdr = values.get("gdrcopy", {})
    add_os_variants(
        gdr.get("repository", ""),
        gdr.get("image", ""),
        (gdr.get("version") or "").strip(),
    )

    # vGPU Manager – main image is user-supplied (repository/version empty); skip.
    # The driverManager sidecar is pinned.
    vgpu = values.get("vgpuManager", {})
    vgpu_dm = vgpu.get("driverManager", {})
    add(vgpu_dm.get("repository", ""), vgpu_dm.get("image", ""), vgpu_dm.get("version", ""))

    # vGPU Device Manager
    vdm = values.get("vgpuDeviceManager", {})
    add(vdm.get("repository", ""), vdm.get("image", ""), vdm.get("version", ""))

    # VFIO Manager (and its driverManager sidecar)
    vfio = values.get("vfioManager", {})
    add(vfio.get("repository", ""), vfio.get("image", ""), vfio.get("version", ""))
    vfio_dm = vfio.get("driverManager", {})
    add(vfio_dm.get("repository", ""), vfio_dm.get("image", ""), vfio_dm.get("version", ""))

    # Sandbox Device Plugin (KubeVirt GPU passthrough)
    sdp = values.get("sandboxDevicePlugin", {})
    add(sdp.get("repository", ""), sdp.get("image", ""), sdp.get("version", ""))

    # Kata Sandbox Device Plugin
    ksdp = values.get("kataSandboxDevicePlugin", {})
    add(ksdp.get("repository", ""), ksdp.get("image", ""), ksdp.get("version", ""))

    # Confidential Computing Manager
    cc = values.get("ccManager", {})
    add(cc.get("repository", ""), cc.get("image", ""), cc.get("version", ""))

    # kataManager has no image fields in values.yaml (operator-managed); skip.

    return sorted(images)


# ---------------------------------------------------------------------------
# NFD subchart image extraction
# ---------------------------------------------------------------------------

def _extract_nfd_images(nfd_values: dict, nfd_chart: dict) -> list[str]:
    """Return the NFD image reference from the bundled NFD subchart."""
    img_cfg = nfd_values.get("image", {})
    repository = (img_cfg.get("repository") or "").strip()
    # NFD uses a single image for all its components (master, worker, gc).
    # The tag defaults to Chart.AppVersion when not explicitly set.
    tag = (img_cfg.get("tag") or "").strip()
    if not tag:
        tag = (nfd_chart.get("appVersion") or "").strip()
    # NFD's repository already contains the image name (no separate 'image' key)
    if repository and tag:
        return [f"{repository}:{tag}"]
    return []


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def _parse_args() -> argparse.Namespace:
    script_dir = os.path.dirname(os.path.abspath(__file__))
    repo_root = os.path.join(script_dir, "..", "..")
    chart_dir = os.path.join(repo_root, "deployments", "gpu-operator")
    nfd_dir = os.path.join(chart_dir, "charts", "node-feature-discovery")

    parser = argparse.ArgumentParser(
        description="Generate a list of all container images required by the GPU Operator.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--values",
        default=os.path.join(chart_dir, "values.yaml"),
        metavar="PATH",
        help="Path to gpu-operator values.yaml",
    )
    parser.add_argument(
        "--chart",
        default=os.path.join(chart_dir, "Chart.yaml"),
        metavar="PATH",
        help="Path to gpu-operator Chart.yaml",
    )
    parser.add_argument(
        "--nfd-values",
        default=os.path.join(nfd_dir, "values.yaml"),
        metavar="PATH",
        help="Path to the bundled NFD subchart values.yaml",
    )
    parser.add_argument(
        "--nfd-chart",
        default=os.path.join(nfd_dir, "Chart.yaml"),
        metavar="PATH",
        help="Path to the bundled NFD subchart Chart.yaml",
    )
    parser.add_argument(
        "--output",
        default=None,
        metavar="PATH",
        help="Write image list to PATH instead of stdout",
    )
    parser.add_argument(
        "--no-nfd",
        action="store_true",
        help="Exclude NFD subchart images from the output",
    )
    parser.add_argument(
        "--skip-registry",
        action="store_true",
        help="Skip registry tag lookups; use the version from values.yaml as-is "
             "(no OS-variant expansion for driver/gds/gdrcopy)",
    )
    parser.add_argument(
        "--gpu-operator-version",
        default=None,
        metavar="VERSION",
        help="Override the gpu-operator image version (e.g., v1.0.0)",
    )
    return parser.parse_args()


def main() -> None:
    args = _parse_args()

    # Load primary chart files
    values = _load_yaml(args.values)
    chart = _load_yaml(args.chart)

    # Resolve the operator appVersion
    app_version = (chart.get("appVersion") or "").strip()
    if not app_version:
        print("Error: Could not determine appVersion. "
              "Ensure Chart.yaml contains an appVersion field.",
              file=sys.stderr)
        sys.exit(1)

    # Collect GPU Operator component images
    all_images: list[str] = _extract_operator_images(
        values, app_version, args.skip_registry, args.gpu_operator_version
    )

    # Collect NFD images (from the bundled subchart)
    if not args.no_nfd:
        try:
            nfd_values = _load_yaml(args.nfd_values)
            nfd_chart = _load_yaml(args.nfd_chart)
            all_images += _extract_nfd_images(nfd_values, nfd_chart)
        except FileNotFoundError as exc:
            print(f"Warning: NFD chart not found ({exc}); skipping NFD images. "
                  "Run 'helm dependency update deployments/gpu-operator' to fetch it, "
                  "or pass --no-nfd to suppress this warning.",
                  file=sys.stderr)

    # Deduplicate and sort
    all_images = sorted(set(all_images))

    output_text = "\n".join(all_images) + "\n"

    if args.output:
        with open(args.output, "w") as fh:
            fh.write(output_text)
    else:
        sys.stdout.write(output_text)


if __name__ == "__main__":
    main()
