# Kagenti SPARC reflection service.
# Depends on the agent-lifecycle-toolkit (ALTK) package straight from PyPI — no vendoring.
FROM python:3.12-slim AS base

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    LITELLM_LOCAL_MODEL_COST_MAP=True

WORKDIR /app

# Install dependencies first for better layer caching.
COPY pyproject.toml README.md ./
COPY sparc_service ./sparc_service
RUN pip install --upgrade pip && pip install .

# Drop privileges.
RUN useradd --create-home --uid 10001 sparc
USER sparc

EXPOSE 8090

# PORT/HOST and SPARC_* / WX_* are provided via the Deployment env (ConfigMap/Secret).
CMD ["python", "-m", "sparc_service"]
