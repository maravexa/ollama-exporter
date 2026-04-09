#!/bin/sh
set -e

# Create system group if it doesn't exist
if ! getent group ollama-exporter > /dev/null 2>&1; then
    groupadd --system ollama-exporter
fi

# Create system user if it doesn't exist
if ! getent passwd ollama-exporter > /dev/null 2>&1; then
    useradd --system \
        --gid ollama-exporter \
        --no-create-home \
        --shell /usr/sbin/nologin \
        --comment "Ollama Exporter system user" \
        ollama-exporter
fi

systemctl daemon-reload
systemctl enable ollama-exporter
