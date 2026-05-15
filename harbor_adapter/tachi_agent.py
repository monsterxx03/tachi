"""
Tachi Agent — Harbor adapter for Terminal-Bench 2.0.

Usage:
    # Build Tachi for Linux first:
    make build-linux

    # Run Terminal-Bench with Tachi:
    export ANTHROPIC_API_KEY="sk-..."
    harbor run --dataset terminal-bench@2.0 \\
        --agent-import-path ./harbor_adapter/tachi_agent.py:TachiAgent \\
        --model anthropic/claude-sonnet-4-20250514 \\
        --n-concurrent 4

    # With a custom binary path:
    export TACHI_BINARY_PATH="./tachi-linux-amd64"
    harbor run --dataset terminal-bench@2.0 \\
        --agent-import-path ./harbor_adapter/tachi_agent.py:TachiAgent \\
        --model anthropic/claude-sonnet-4-20250514

    # Override default timeout and max iterations:
    harbor run ... \\
        --agent-import-path ...:TachiAgent \\
        --ak max_iterations=100 \\
        --ak timeout=15m
"""

import json
import os
import shlex
from pathlib import Path

from harbor.agents.installed.base import (
    BaseInstalledAgent,
    CliFlag,
    EnvVar,
    with_prompt_template,
)
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext


class TachiAgent(BaseInstalledAgent):
    """Harbor adapter for the Tachi AI agent.

    Tachi is a Go binary that runs as a single-turn CLI agent.
    This adapter installs the binary into the benchmark container,
    executes it with the task instruction, and parses the JSON output
    to extract token usage and completion status.
    """

    CLI_FLAGS = [
        CliFlag(
            "max_iterations",
            cli="--max-iterations",
            type="int",
            default=50,
            env_fallback="TACHI_MAX_ITERATIONS",
        ),
        CliFlag(
            "timeout",
            cli="--timeout",
            type="str",
            default="10m",
            env_fallback="TACHI_TIMEOUT",
        ),
    ]

    ENV_VARS = [
        EnvVar(
            "max_iterations",
            env="TACHI_MAX_ITERATIONS",
            type="int",
            env_fallback="TACHI_MAX_ITERATIONS",
        ),
    ]

    # ── Identity ──────────────────────────────────────────────────────

    @staticmethod
    def name() -> str:
        return "tachi"

    def version(self) -> str | None:
        return self._version

    def get_version_command(self) -> str | None:
        return "tachi --version 2>/dev/null"

    def parse_version(self, stdout: str) -> str:
        # "tachi version abc1234" -> "abc1234"
        text = stdout.strip()
        for prefix in ("tachi version ", "tachi "):
            if text.startswith(prefix):
                return text[len(prefix):].strip()
        return text

    # ── Install ───────────────────────────────────────────────────────

    async def install(self, environment: BaseEnvironment) -> None:
        """Install the tachi binary and create its config inside the container."""

        # 1. Ensure required system tools
        await self.exec_as_root(
            environment,
            command=(
                "if command -v apk &> /dev/null; then"
                "  apk add --no-cache curl;"
                " elif command -v apt-get &> /dev/null; then"
                "  apt-get update -qq && apt-get install -y -qq curl;"
                " elif command -v yum &> /dev/null; then"
                "  yum install -y curl;"
                " fi"
            ),
            env={"DEBIAN_FRONTEND": "noninteractive"},
        )

        # 2. Install the tachi binary
        binary_url = os.environ.get("TACHI_BINARY_URL", "")
        binary_path = os.environ.get("TACHI_BINARY_PATH", "")

        if binary_url:
            # Download from URL (e.g. GitHub release)
            await self.exec_as_root(
                environment,
                command=(
                    f"curl -fsSL {shlex.quote(binary_url)} -o /usr/local/bin/tachi "
                    "&& chmod +x /usr/local/bin/tachi"
                ),
            )
        elif binary_path:
            # Upload from local filesystem
            src = Path(binary_path).expanduser().resolve()
            if not src.exists():
                raise RuntimeError(
                    f"TACHI_BINARY_PATH={binary_path} not found. "
                    "Build it first: make build-linux"
                )
            await environment.upload_file(str(src), "/usr/local/bin/tachi")
            await self.exec_as_root(environment, "chmod +x /usr/local/bin/tachi")
        else:
            # Fallback: look for tachi-linux-amd64 in the current directory
            cwd_binary = Path.cwd() / "tachi-linux-amd64"
            if cwd_binary.exists():
                await environment.upload_file(str(cwd_binary), "/usr/local/bin/tachi")
                await self.exec_as_root(environment, "chmod +x /usr/local/bin/tachi")
            else:
                raise RuntimeError(
                    "No Tachi binary found. Set TACHI_BINARY_PATH, TACHI_BINARY_URL, "
                    "or place tachi-linux-amd64 in the current directory."
                )

        # 3. Create ~/.tachi/ config directory
        await self.exec_as_agent(environment, "mkdir -p ~/.tachi")

        # 4. Generate config.yaml that uses environment variables for API keys.
        #    The model name is derived from self.model_name (passed via --model).
        config_yaml = self._generate_config_yaml()
        # Use heredoc to write multi-line content safely
        await self.exec_as_agent(
            environment,
            command=(
                f"cat > ~/.tachi/config.yaml << 'TACHI_EOF'\n"
                f"{config_yaml}\n"
                f"TACHI_EOF"
            ),
        )

        # 5. Verify installation
        try:
            result = await environment.exec(
                command="tachi --version 2>&1",
                timeout_sec=10,
            )
            self.logger.info(
                "Tachi installed: %s",
                result.stdout.strip() if result.stdout else "unknown version",
            )
        except Exception as exc:
            self.logger.warning("Tachi version check failed: %s", exc)

    def _generate_config_yaml(self) -> str:
        """Generate a config.yaml that sources API keys from env vars.

        The format uses Tachi's env-var substitution syntax ${VAR_NAME}.
        """
        model = self.model_name or "claude-sonnet-4-20250514"
        model_clean = model

        # Strip provider prefix for the config's model field
        if "/" in model:
            model_clean = model.split("/", 1)[1]

        # Determine provider type
        provider_type = "openai"
        api_key_var = "${OPENAI_API_KEY}"
        if model.startswith("claude") or model.startswith("anthropic/"):
            provider_type = "anthropic"
            api_key_var = "${ANTHROPIC_API_KEY}"

        return f"""# Auto-generated by Tachi Harbor adapter
language: en
default_provider: bench
providers:
  - name: bench
    type: {provider_type}
    model: {model_clean}
    api_key: "{api_key_var}"
"""

    # ── Run ───────────────────────────────────────────────────────────

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        """Execute tachi with the task instruction inside the container.

        The JSON output is stored for parsing in populate_context_post_run.
        We call environment.exec directly (not exec_as_agent) because Tachi
        uses exit codes to signal completion status (code 0 = success,
        code 1 = error, code 2 = budget exhausted), and we want to capture
        the output even on non-zero exits.
        """
        cli_flags = self.build_cli_flags()
        escaped_instruction = shlex.quote(instruction)

        # Build environment: pass through API keys from the host
        env = {}
        if os.environ.get("ANTHROPIC_API_KEY"):
            env["ANTHROPIC_API_KEY"] = os.environ["ANTHROPIC_API_KEY"]
        if os.environ.get("OPENAI_API_KEY"):
            env["OPENAI_API_KEY"] = os.environ["OPENAI_API_KEY"]
        if os.environ.get("ANTHROPIC_BASE_URL"):
            env["ANTHROPIC_BASE_URL"] = os.environ["ANTHROPIC_BASE_URL"]
        if os.environ.get("OPENAI_BASE_URL"):
            env["OPENAI_BASE_URL"] = os.environ["OPENAI_BASE_URL"]

        command = f"tachi run --json {cli_flags} --prompt {escaped_instruction}".strip()

        self.logger.info("Running: %s", command)

        result = await environment.exec(
            command=command,
            env=env or None,
            timeout_sec=self._parse_timeout(),
        )

        # Store stdout for post-run parsing, even on non-zero exit
        self._last_result = result

    def _parse_timeout(self) -> int:
        """Parse the configured timeout into seconds."""
        raw = self._resolved_flags.get("timeout", "10m")
        if isinstance(raw, (int, float)):
            return int(raw)
        raw = str(raw).strip().lower()
        if raw.endswith("h"):
            return int(float(raw[:-1]) * 3600)
        if raw.endswith("m"):
            return int(float(raw[:-1]) * 60)
        if raw.endswith("s"):
            return int(float(raw[:-1]))
        try:
            return int(raw)
        except ValueError:
            return 600  # default 10 minutes

    # ── Post-run parsing ──────────────────────────────────────────────

    def populate_context_post_run(self, context: AgentContext) -> None:
        """Parse Tachi's JSON output and populate token counts."""
        if not hasattr(self, "_last_result") or self._last_result is None:
            self.logger.debug("No result captured from Tachi run")
            return

        stdout = (self._last_result.stdout or "").strip()
        stderr = (self._last_result.stderr or "").strip()
        return_code = self._last_result.return_code

        if not stdout:
            self.logger.debug(
                "Tachi produced no stdout (rc=%d, stderr=%s)",
                return_code,
                stderr[:200] if stderr else "none",
            )
            return

        # Parse JSON
        try:
            data = json.loads(stdout)
        except json.JSONDecodeError as exc:
            self.logger.debug(
                "Failed to parse Tachi JSON output: %s\nstdout=%s",
                exc,
                stdout[:500],
            )
            return

        # Extract token usage
        usage = data.get("usage")
        if usage and isinstance(usage, dict):
            context.n_input_tokens = usage.get("input_tokens", 0) or 0
            context.n_output_tokens = usage.get("output_tokens", 0) or 0
            cache_read = usage.get("cache_read_input_tokens", 0) or 0
            cache_creation = usage.get("cache_creation_input_tokens", 0) or 0
            context.n_cache_tokens = cache_read + cache_creation

        # Store execution metadata
        context.metadata = {
            "exit_reason": data.get("exit_reason", "unknown"),
            "iterations_used": data.get("iterations_used", 0),
            "error": data.get("error"),
            "return_code": return_code,
        }

        self.logger.info(
            "Tachi finished: exit_reason=%s, iterations=%d, "
            "input_tokens=%s, output_tokens=%s, cache_tokens=%s",
            data.get("exit_reason"),
            data.get("iterations_used"),
            context.n_input_tokens,
            context.n_output_tokens,
            context.n_cache_tokens,
        )
