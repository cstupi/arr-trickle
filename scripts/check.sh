#!/bin/sh
# Lint, type-check and unit-test in a throwaway Python container (same steps as CI).
set -eu
cd "$(dirname "$0")/.."
exec docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$PWD":/src -w /src python:3.12.7-slim sh -euc '
python -m venv /tmp/v >/dev/null && . /tmp/v/bin/activate
pip install -q --disable-pip-version-check -e ".[dev]"
ruff format --check src tests
ruff check src tests
mypy
pytest -q -p no:cacheprovider --cov
'
