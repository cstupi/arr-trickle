# syntax=docker/dockerfile:1
# Pinned base; Dependabot proposes updates (see .github/dependabot.yml).
FROM python:3.13.3-alpine3.20

LABEL org.opencontainers.image.title="arr-trickle" \
      org.opencontainers.image.description="Work through the Sonarr/Radarr/Lidarr backlog a few searches at a time" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/OWNER/arr-trickle"

# Standard library only: the package is copied straight into site-packages, so the
# image has no pip-installed code at all.
COPY src/arr_trickle /usr/local/lib/python3.12/site-packages/arr_trickle

# Runs as nobody unless overridden (e.g. `user: "1000:1000"` to read your *arr config.xml).
USER 65534:65534
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1
VOLUME ["/state"]

HEALTHCHECK --interval=5m --timeout=10s --start-period=2m \
    CMD ["python3", "-I", "-m", "arr_trickle", "healthcheck"]
ENTRYPOINT ["python3", "-I", "-m", "arr_trickle"]
CMD ["run"]
