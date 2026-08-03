"""Console/`python -m sparc_service` entry point."""

from __future__ import annotations

import uvicorn

from .settings import Settings


def main() -> None:
    import logging
    import os
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s:%(name)s:%(message)s", datefmt="%Y-%m-%dT%H:%M:%SZ")
    # Demote noisy third-party loggers — their INFO adds no operational value
    logging.getLogger("LiteLLM").setLevel(logging.WARNING)
    if os.getenv("SPARC_DEBUG_LLM", "").strip().lower() in ("1", "true", "yes"):
        logging.getLogger("sparc_service.llm_debug").setLevel(logging.DEBUG)
        logging.getLogger("altk").setLevel(logging.DEBUG)
    settings = Settings.from_env()
    uvicorn.run("sparc_service.api:app", host=settings.host, port=settings.port, log_level="info", access_log=False)


if __name__ == "__main__":
    main()
