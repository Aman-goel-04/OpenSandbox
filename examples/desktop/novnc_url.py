# Copyright 2025 Alibaba Group Holding Ltd.
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

"""Helpers for configuring and building browser-ready noVNC URLs."""

from urllib.parse import urlencode, urlsplit, urlunsplit


def normalize_domain(domain: str) -> str:
    """Normalize whitespace and a case-insensitive HTTP scheme prefix."""
    normalized = domain.strip()
    lowered = normalized.lower()
    if lowered.startswith("https://"):
        return f"https://{normalized[len('https://') :]}"
    if lowered.startswith("http://"):
        return f"http://{normalized[len('http://') :]}"
    return normalized


def resolve_protocol(domain: str, configured_protocol: str | None) -> str:
    """Resolve the URL protocol, honoring a scheme embedded in the domain."""
    lowered_domain = normalize_domain(domain).lower()
    if lowered_domain.startswith("https://"):
        return "https"
    if lowered_domain.startswith("http://"):
        return "http"
    return configured_protocol.lower() if configured_protocol else "http"


def parse_bool(value: str, name: str) -> bool:
    """Parse an environment-style boolean value."""
    normalized = value.strip().lower()
    if normalized in {"1", "true", "yes", "on"}:
        return True
    if normalized in {"0", "false", "no", "off"}:
        return False
    raise RuntimeError(f"{name} must be one of: 1, true, yes, on, 0, false, no, off")


def resolve_novnc_protocol(management_protocol: str, use_server_proxy: bool) -> str:
    """Use the management scheme only when noVNC shares the server origin."""
    return management_protocol if use_server_proxy else "http"


def build_novnc_url(endpoint: str, protocol: str) -> str:
    """Build a noVNC page URL whose WebSocket targets the same endpoint."""
    scheme = protocol.lower()
    if scheme not in {"http", "https"}:
        raise ValueError("protocol must be 'http' or 'https'")

    parsed = urlsplit(f"{scheme}://{endpoint}")
    if not parsed.hostname:
        raise ValueError("endpoint must contain a hostname")

    port = parsed.port or (443 if scheme == "https" else 80)
    proxy_path = parsed.path.strip("/")
    page_path = f"{parsed.path.rstrip('/')}/vnc.html"
    if not page_path.startswith("/"):
        page_path = f"/{page_path}"

    query = urlencode(
        {"host": parsed.hostname, "port": port, "path": proxy_path},
        safe="/",
    )
    return urlunsplit((scheme, parsed.netloc, page_path, query, ""))
