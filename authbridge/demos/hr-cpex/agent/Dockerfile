# hr-cpex agent — A2A server (a2a-sdk) wrapping a litellm tool-calling
# loop over the hr-mcp tools. Its authbridge-cpex sidecar enforces CPEX
# on the agent's outbound MCP calls.
#
# Built locally and loaded into kind:
#   docker build -t hr-cpex-agent:dev .
#   kind load docker-image hr-cpex-agent:dev --name rossoctl
# The Deployment uses imagePullPolicy: Never so the kubelet takes the
# locally-loaded image rather than pulling from a registry.

# Pinned by digest (multi-arch index) to match the SHA-pinned authbridge-cpex
# image in this PR; bump deliberately rather than floating on the tag.
FROM python:3.12-slim@sha256:d764629ce0ddd8c71fd371e9901efb324a95789d2315a47db7e4d27e78f1b0e9

WORKDIR /app

# Server deps only (requirements.txt). requirements-client.txt is for the
# host-side chat.py and is intentionally not installed here.
COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

COPY agent.py /app/agent.py

# A2A server port. The Deployment sets PORT=8001; the sidecar's reverse
# proxy fronts it and the Service exposes the sidecar.
EXPOSE 8001

CMD ["python", "agent.py"]
