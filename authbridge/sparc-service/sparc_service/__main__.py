"""Console/`python -m sparc_service` entry point."""

from __future__ import annotations

import uvicorn

from .settings import Settings


def main() -> None:
    settings = Settings.from_env()
    uvicorn.run("sparc_service.api:app", host=settings.host, port=settings.port, log_level="info")


if __name__ == "__main__":
    main()
