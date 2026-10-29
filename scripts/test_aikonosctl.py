"""
Unit tests for scripts/aikonosctl pure helpers.

No live Docker — all subprocess calls are monkeypatched.
"""
from __future__ import annotations

import importlib.util
import json
import sys
import types
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

# ── Load the script as a module without executing main() ─────────────────────

_SCRIPT = Path(__file__).resolve().parent / "aikonosctl"


def _load_aikonosctl() -> types.ModuleType:
    # The script has no .py extension; provide the loader explicitly so Python
    # doesn't try to infer it from a missing suffix.
    loader = importlib.machinery.SourceFileLoader("aikonosctl", str(_SCRIPT))
    spec = importlib.util.spec_from_loader("aikonosctl", loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    return mod


aikonosctl = _load_aikonosctl()

# ── C1: compose_exec arg construction ────────────────────────────────────────


class TestComposeExec:
    def test_argv_structure_capture(self):
        """compose_exec builds [docker, compose, exec, -T, svc, *cmd] for capture=True."""
        fake_result = MagicMock()
        fake_result.returncode = 0
        fake_result.stdout = "output\n"

        with patch("subprocess.run", return_value=fake_result) as mock_run:
            out = aikonosctl.compose_exec("postgres", "psql", "-U", "aikonos", capture=True)

        mock_run.assert_called_once()
        argv = mock_run.call_args[0][0]
        assert argv == ["docker", "compose", "exec", "-T", "postgres", "psql", "-U", "aikonos"]
        assert out == "output"

    def test_argv_structure_non_capture(self):
        """capture=False runs without capture_output, returns empty string."""
        with patch("subprocess.run", return_value=MagicMock(returncode=0)) as mock_run:
            out = aikonosctl.compose_exec("minio", "mc", "watch", "local/aikonos-audit/", capture=False)

        argv = mock_run.call_args[0][0]
        assert argv == ["docker", "compose", "exec", "-T", "minio", "mc", "watch", "local/aikonos-audit/"]
        assert out == ""

    def test_non_zero_exit_calls_sys_exit(self):
        """Non-zero returncode exits the process."""
        fake_result = MagicMock()
        fake_result.returncode = 1
        fake_result.stderr = "exec failed"

        with patch("subprocess.run", return_value=fake_result):
            with pytest.raises(SystemExit):
                aikonosctl.compose_exec("postgres", "psql", "-c", "SELECT 1")

    def test_cwd_is_repo_root(self):
        """cwd passed to subprocess.run is the repo root (parent of scripts/)."""
        fake_result = MagicMock()
        fake_result.returncode = 0
        fake_result.stdout = ""

        with patch("subprocess.run", return_value=fake_result) as mock_run:
            aikonosctl.compose_exec("broker", "ls")

        kwargs = mock_run.call_args[1]
        assert "cwd" in kwargs
        expected_root = Path(_SCRIPT).resolve().parent.parent
        assert Path(kwargs["cwd"]) == expected_root


# ── C3: parse_compose_ps ─────────────────────────────────────────────────────

# Minimal representative sample: one service with health, one without, one exited
_SAMPLE_PS = "\n".join([
    json.dumps({"Service": "broker",        "State": "running", "Health": ""}),
    json.dumps({"Service": "agent-gateway", "State": "running", "Health": "healthy"}),
    json.dumps({"Service": "postgres",      "State": "running", "Health": "healthy"}),
    json.dumps({"Service": "migrate",       "State": "exited",  "Health": ""}),
])


class TestParseComposePs:
    def test_returns_list_of_dicts(self):
        result = aikonosctl.parse_compose_ps(_SAMPLE_PS)
        assert isinstance(result, list)
        assert len(result) == 4

    def test_extracts_name_state_health(self):
        result = aikonosctl.parse_compose_ps(_SAMPLE_PS)
        broker = next(r for r in result if r["name"] == "broker")
        assert broker["state"] == "running"
        assert broker["health"] == ""

        gw = next(r for r in result if r["name"] == "agent-gateway")
        assert gw["health"] == "healthy"

    def test_exited_service_parsed(self):
        result = aikonosctl.parse_compose_ps(_SAMPLE_PS)
        migrate = next(r for r in result if r["name"] == "migrate")
        assert migrate["state"] == "exited"

    def test_empty_stdout_returns_empty_list(self):
        assert aikonosctl.parse_compose_ps("") == []
        assert aikonosctl.parse_compose_ps("   \n  ") == []

    def test_real_compose_json_shape(self):
        """Verify the parser handles the actual multi-field JSON docker compose emits."""
        real_line = json.dumps({
            "Service": "vault",
            "State": "running",
            "Health": "healthy",
            "Status": "Up 15 minutes (healthy)",
            "Name": "aikonos-vault-1",
            "Ports": "0.0.0.0:8200->8200/tcp",
        })
        result = aikonosctl.parse_compose_ps(real_line)
        assert result[0]["name"] == "vault"
        assert result[0]["state"] == "running"
        assert result[0]["health"] == "healthy"

    def test_malformed_line_skipped_valid_lines_parsed(self):
        """Malformed/non-JSON lines are skipped; valid lines before and after are returned."""
        good_before = json.dumps({"Service": "broker", "State": "running", "Health": ""})
        good_after  = json.dumps({"Service": "nats",   "State": "running", "Health": "healthy"})
        stdout = "\n".join([
            good_before,
            "not-json{{{",
            "   ",
            "plain text line",
            good_after,
        ])
        result = aikonosctl.parse_compose_ps(stdout)
        # Only the two well-formed lines survive.
        assert len(result) == 2
        names = [r["name"] for r in result]
        assert "broker" in names
        assert "nats"   in names


# ── F117/F231: psql values are bound, never spliced; kill-switch tenant ──────


def _run_main(argv: list[str]) -> MagicMock:
    """Run main() with argv; return the subprocess.run mock."""
    fake = MagicMock(returncode=0, stdout="", stderr="")
    with patch.object(sys, "argv", ["aikonosctl", *argv]), \
         patch("subprocess.run", return_value=fake) as mock_run:
        aikonosctl.main()
    return mock_run


class TestPsqlBinding:
    def test_task_get_binds_task_id(self):
        tid = "0f8fad5b-d9cb-469f-a165-70867728950e"
        mock_run = _run_main(["task", "get", tid])
        argv = mock_run.call_args[0][0]
        sql = mock_run.call_args[1]["input"]
        assert f"task_id={tid}" in argv
        assert ":'task_id'" in sql and tid not in sql

    def test_task_id_injection_refused(self):
        with pytest.raises(SystemExit):
            _run_main(["task", "approve", "x'); DELETE FROM kill_switches;--"])

    def test_kill_switch_user_requires_tenant(self, monkeypatch):
        # No silent dev-tenant fallback, even with AIKONOS_TENANT_ID set.
        monkeypatch.setenv("AIKONOS_TENANT_ID", "11111111-1111-1111-1111-111111111111")
        with pytest.raises(SystemExit):
            _run_main(["kill-switch", "user", "alice"])

    def test_kill_switch_user_binds_tenant_and_subject(self):
        tenant = "6c3b1e4a-0d2f-4a8e-9b7c-1f2e3d4c5b6a"
        mock_run = _run_main(["kill-switch", "user", "--tenant", tenant, "alice@corp"])
        argv = mock_run.call_args[0][0]
        sql = mock_run.call_args[1]["input"]
        assert f"tenant={tenant}" in argv and "subject=alice@corp" in argv
        assert tenant not in sql and "alice@corp" not in sql

    def test_kill_switch_user_subject_injection_refused(self):
        with pytest.raises(SystemExit):
            _run_main(["kill-switch", "user", "--tenant",
                       "6c3b1e4a-0d2f-4a8e-9b7c-1f2e3d4c5b6a", "x'); DELETE FROM kill_switches;--"])

    def test_kill_switch_tenant_requires_uuid(self):
        with pytest.raises(SystemExit):
            _run_main(["kill-switch", "tenant", "aikonos-dev"])


class TestAuditVerifyCredentials:
    def test_no_default_minio_credentials(self):
        # printenv returns empty: refuse rather than fall back to minioadmin.
        with pytest.raises(SystemExit):
            _run_main(["audit", "verify"])
