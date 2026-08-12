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

import unittest

from novnc_url import build_novnc_url, resolve_protocol


class BuildNoVNCURLTest(unittest.TestCase):
    def test_resolves_protocol_from_domain_url(self) -> None:
        self.assertEqual(resolve_protocol("https://sandbox.example.com", None), "https")

    def test_domain_url_scheme_overrides_explicit_protocol(self) -> None:
        self.assertEqual(
            resolve_protocol("https://sandbox.example.com", "HTTP"),
            "https",
        )

    def test_direct_http_endpoint(self) -> None:
        self.assertEqual(
            build_novnc_url("192.168.0.104:41618/proxy/6080", "http"),
            "http://192.168.0.104:41618/proxy/6080/vnc.html"
            "?host=192.168.0.104&port=41618&path=proxy/6080",
        )

    def test_https_server_proxy_uses_default_tls_port(self) -> None:
        self.assertEqual(
            build_novnc_url("sandbox.example.com/v1/sandboxes/sbx-123/proxy/6080", "https"),
            "https://sandbox.example.com/v1/sandboxes/sbx-123/proxy/6080/vnc.html"
            "?host=sandbox.example.com&port=443"
            "&path=v1/sandboxes/sbx-123/proxy/6080",
        )

    def test_https_server_proxy_preserves_explicit_port(self) -> None:
        self.assertIn(
            "host=sandbox.example.com&port=8443",
            build_novnc_url(
                "sandbox.example.com:8443/v1/sandboxes/sbx-123/proxy/6080",
                "https",
            ),
        )

    def test_rejects_unknown_protocol(self) -> None:
        with self.assertRaisesRegex(ValueError, "protocol"):
            build_novnc_url("sandbox.example.com/proxy/6080", "ftp")


if __name__ == "__main__":
    unittest.main()
