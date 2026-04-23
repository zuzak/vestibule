FROM python:3.12-slim AS build

WORKDIR /src

# Copy just the project files so dependency resolution caches well.
COPY pyproject.toml README.md ./
COPY src ./src

# Build a wheel without requiring network at runtime. pip install creates
# the wheel in a staging venv so we can copy site-packages cleanly.
RUN pip install --no-cache-dir --root-user-action=ignore \
      --prefix=/install .

FROM python:3.12-slim

# Non-root user. Matches Vestibule's non-root posture.
RUN useradd --system --uid 65532 --home /nonexistent --shell /usr/sbin/nologin mcp

COPY --from=build /install /usr/local

USER 65532:65532

EXPOSE 8080 9090

ENTRYPOINT ["vestibule-mcp"]
