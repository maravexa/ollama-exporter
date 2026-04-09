#!/bin/sh
set -e

systemctl stop ollama-exporter || true
systemctl disable ollama-exporter || true
