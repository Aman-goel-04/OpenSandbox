# Copyright 2026 Alibaba Group Holding Ltd.
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

from fastapi.testclient import TestClient

from opensandbox_server.api import devops


def test_diagnostics_logs_with_scope_returns_stable_inline_descriptor(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    content = "sandbox log: 测试"

    class StubService:
        @staticmethod
        def get_sandbox_logs(
            sandbox_id: str,
            tail: int,
            since: str | None = None,
            container: str | None = None,
        ) -> str:
            assert sandbox_id == "sbx-001"
            assert tail == 101
            assert since is None
            assert container is None
            return content

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?scope=container",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.headers["content-type"].startswith("application/json")
    assert response.json() == {
        "sandboxId": "sbx-001",
        "kind": "logs",
        "scope": "container",
        "delivery": "inline",
        "contentType": "text/plain; charset=utf-8",
        "content": content,
        "contentLength": len(content.encode("utf-8")),
        "truncated": False,
    }


def test_diagnostics_logs_reports_and_applies_line_limit(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    lines = [f"line {index}" for index in range(101)]

    class StubService:
        @staticmethod
        def get_sandbox_logs(
            sandbox_id: str,
            tail: int,
            since: str | None = None,
            container: str | None = None,
        ) -> str:
            assert sandbox_id == "sbx-001"
            assert tail == 101
            assert since is None
            assert container is None
            return "\n".join(lines)

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?scope=container",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["content"].splitlines() == lines[-100:]
    assert response.json()["truncated"] is True


def test_diagnostics_logs_with_scope_ignores_legacy_container_selector(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    containers: list[str | None] = []

    class StubService:
        @staticmethod
        def get_sandbox_logs(
            sandbox_id: str,
            tail: int,
            since: str | None = None,
            container: str | None = None,
        ) -> str:
            containers.append(container)
            return "sandbox logs"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    for scope in ("container", "all"):
        response = client.get(
            f"/v1/sandboxes/sbx-001/diagnostics/logs?scope={scope}&container=egress",
            headers=auth_headers,
        )

        assert response.status_code == 200
        assert response.json()["scope"] == scope

    assert containers == [None, None]


def test_diagnostics_logs_with_scope_ignores_legacy_since_filter(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    since_values: list[str | None] = []

    class StubService:
        @staticmethod
        def get_sandbox_logs(
            sandbox_id: str,
            tail: int,
            since: str | None = None,
            container: str | None = None,
        ) -> str:
            since_values.append(since)
            return "sandbox logs"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    for scope in ("container", "all"):
        response = client.get(
            f"/v1/sandboxes/sbx-001/diagnostics/logs?scope={scope}&since=5m",
            headers=auth_headers,
        )

        assert response.status_code == 200
        assert response.json()["scope"] == scope

    assert since_values == [None, None]


def test_diagnostics_logs_with_scope_ignores_legacy_tail_bound(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    tails: list[int] = []

    class StubService:
        @staticmethod
        def get_sandbox_logs(
            sandbox_id: str,
            tail: int,
            since: str | None = None,
            container: str | None = None,
        ) -> str:
            tails.append(tail)
            return "first line\nsecond line"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    for scope in ("container", "all"):
        for tail in (1, 10000):
            response = client.get(
                f"/v1/sandboxes/sbx-001/diagnostics/logs?scope={scope}&tail={tail}",
                headers=auth_headers,
            )

            assert response.status_code == 200
            assert response.json()["content"] == "first line\nsecond line"

    assert tails == [101, 101, 101, 101]


def test_diagnostics_logs_rejects_unsupported_scope(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_logs(*args, **kwargs) -> str:
            raise AssertionError("unsupported scope must not query the backend")

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?scope=lifecycle",
        headers=auth_headers,
    )

    assert response.status_code == 400
    assert response.json() == {
        "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
        "message": (
            "Unsupported logs diagnostics scope 'lifecycle'. Supported scopes: container, all."
        ),
    }


def test_diagnostics_logs_all_scope_discloses_backend_limit(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_logs(*args, **kwargs) -> str:
            return "container logs only"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?scope=all",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["warnings"] == [
        "The current backend only contributes sandbox container logs to the all scope."
    ]


def test_diagnostics_logs_without_scope_preserves_deprecated_plain_text(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_logs(
            sandbox_id: str,
            tail: int,
            since: str | None = None,
            container: str | None = None,
        ) -> str:
            assert sandbox_id == "sbx-001"
            assert tail == 25
            assert since == "5m"
            assert container is None
            return "legacy logs"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?tail=25&since=5m",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.headers["content-type"].startswith("text/plain")
    assert response.headers["deprecation"] == "true"
    assert response.text == "legacy logs"


def test_diagnostics_logs_forwards_container_query_to_service(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    captured: dict = {}

    class StubService:
        @staticmethod
        def get_sandbox_logs(
            sandbox_id: str,
            tail: int,
            since: str | None = None,
            container: str | None = None,
        ) -> str:
            captured["container"] = container
            return "sidecar logs"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?container=egress",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.text == "sidecar logs"
    assert captured == {"container": "egress"}


def test_diagnostics_events_with_scope_returns_stable_inline_descriptor(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_events(sandbox_id: str, limit: int) -> str:
            assert sandbox_id == "sbx-001"
            assert limit == 51
            return "runtime event"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=RUNTIME",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.headers["content-type"].startswith("application/json")
    assert response.json() == {
        "sandboxId": "sbx-001",
        "kind": "events",
        "scope": "runtime",
        "delivery": "inline",
        "contentType": "text/plain; charset=utf-8",
        "content": "runtime event",
        "contentLength": 13,
        "truncated": False,
    }


def test_diagnostics_events_reports_and_applies_line_limit(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    events = [f"event {index}" for index in range(51)]

    class StubService:
        @staticmethod
        def get_sandbox_events(sandbox_id: str, limit: int) -> str:
            assert sandbox_id == "sbx-001"
            assert limit == 51
            return "\n".join(events)

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=runtime",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["content"].splitlines() == events[:50]
    assert response.json()["truncated"] is True


def test_diagnostics_events_with_scope_ignores_legacy_limit_bound(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    limits: list[int] = []

    class StubService:
        @staticmethod
        def get_sandbox_events(sandbox_id: str, limit: int) -> str:
            limits.append(limit)
            return "first event\nsecond event"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    for scope in ("runtime", "all"):
        for limit in (1, 500):
            response = client.get(
                f"/v1/sandboxes/sbx-001/diagnostics/events?scope={scope}&limit={limit}",
                headers=auth_headers,
            )

            assert response.status_code == 200
            assert response.json()["content"] == "first event\nsecond event"

    assert limits == [51, 51, 51, 51]


def test_diagnostics_events_rejects_unavailable_lifecycle_scope(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    calls: list[tuple[str, int]] = []

    class StubService:
        @staticmethod
        def get_sandbox_events(sandbox_id: str, limit: int) -> str:
            calls.append((sandbox_id, limit))
            return "runtime event"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=lifecycle",
        headers=auth_headers,
    )

    assert response.status_code == 400
    assert response.json() == {
        "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
        "message": (
            "Unsupported events diagnostics scope 'lifecycle'. "
            "Supported scopes: runtime, all."
        ),
    }
    assert calls == []


def test_diagnostics_events_all_scope_discloses_backend_limit(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_events(sandbox_id: str, limit: int) -> str:
            return "runtime event"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=all",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["warnings"] == [
        "The current backend only contributes runtime events to the all scope."
    ]


def test_diagnostics_events_rejects_unsupported_scope(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_events(*args, **kwargs) -> str:
            raise AssertionError("unsupported scope must not query the backend")

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=network",
        headers=auth_headers,
    )

    assert response.status_code == 400
    assert response.json()["code"] == "DIAGNOSTICS_SCOPE_UNSUPPORTED"


def test_diagnostics_events_without_scope_preserves_deprecated_plain_text(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_events(sandbox_id: str, limit: int) -> str:
            assert sandbox_id == "sbx-001"
            assert limit == 10
            return "legacy events"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?limit=10",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.headers["content-type"].startswith("text/plain")
    assert response.headers["deprecation"] == "true"
    assert response.text == "legacy events"


def test_diagnostics_summary_redacts_unexpected_exception_details(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_inspect(sandbox_id: str) -> str:
            raise RuntimeError("backend secret token")

        @staticmethod
        def get_sandbox_events(sandbox_id: str, limit: int) -> str:
            return "events ok"

        @staticmethod
        def get_sandbox_logs(
            sandbox_id: str,
            tail: int,
            since: str | None = None,
            container: str | None = None,
        ) -> str:
            return "logs ok"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/summary",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert "[error] Failed to collect inspect diagnostics." in response.text
    assert "backend secret token" not in response.text
    assert "events ok" in response.text
    assert "logs ok" in response.text
