FROM python:3.13-slim-bookworm@sha256:a5a7017efc5969e00ae2b91c8eb0631049a569652aa4ffc4501769049ae45c72
RUN apt-get update && apt-get install -y --no-install-recommends libcairo2 fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY converter/requirements.txt .
RUN pip install --no-cache-dir --only-binary=:all: -r requirements.txt
COPY converter/server.py converter/decode.py ./
RUN mkdir -p /run/media && chown 65532:65532 /run/media
USER 65532:65532
ENV PYTHONDONTWRITEBYTECODE=1 OPENBLAS_NUM_THREADS=1 OMP_NUM_THREADS=1
CMD ["python", "-I", "/app/server.py"]
