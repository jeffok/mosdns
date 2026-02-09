# 预装 ca-certificates、pyyaml，避免启动时 apk/pip
FROM python:3.12-alpine
RUN apk add --no-cache ca-certificates \
    && pip install --no-cache-dir pyyaml
WORKDIR /app
# 统一启动流程：merge_config → gen_custom_rules → updater（compose 与 RouterOS 共用）
RUN printf '%s\n' '#!/bin/sh' \
    'python -u /app/merge_config.py || exit 1' \
    'python -u /app/gen_custom_rules.py || exit 1' \
    'exec python -u /app/updater.py' > /entrypoint.sh && chmod +x /entrypoint.sh
CMD ["/entrypoint.sh"]
