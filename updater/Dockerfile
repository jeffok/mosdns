# 预装 ca-certificates、pyyaml，避免启动时 apk/pip
FROM python:3.12-alpine
RUN apk add --no-cache ca-certificates \
    && pip install --no-cache-dir pyyaml
WORKDIR /app
# 启动时由 compose 挂载 /app 为只读，仅需 Python 与依赖
CMD ["python", "-u", "/app/updater.py"]
