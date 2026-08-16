# Deep Agents + OpenSandbox Example

Run a [Deep Agent](https://github.com/langchain-ai/deepagents) whose file and
shell tools execute inside an OpenSandbox sandbox.

This uses the
[`langchain-sandbox-opensandbox`](https://pypi.org/project/langchain-sandbox-opensandbox/)
backend, which adapts the OpenSandbox Python SDK to the Deep Agents
`BaseSandbox` interface. Every `ls` / `read_file` / `write_file` / `glob` /
`grep` / command the agent performs is sandboxed.

## Install

```bash
pip install deepagents langchain-sandbox-opensandbox
```

## Run

Point the SDK at a running OpenSandbox server and provide a model key:

```bash
export SANDBOX_DOMAIN=localhost:8080
export SANDBOX_PROTOCOL=http
# export SANDBOX_API_KEY=...        # if your server requires auth
export ANTHROPIC_API_KEY=...        # for the default Deep Agents model

python main.py
```
