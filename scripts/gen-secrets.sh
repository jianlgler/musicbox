#!/usr/bin/env bash
# Generates local secrets. Safe to re-run: existing values are kept, only missing ones are added.
set -euo pipefail
umask 077   # every file this script creates is readable by its owner only

cd "$(dirname "$0")/.."   # work from the repo root, wherever the script is called from

env_file=.env
s3_config=seaweedfs/s3.json

touch "$env_file"

# add_var NAME VALUE: appends NAME=VALUE unless NAME is already in .env
add_var() {
  if grep -q "^$1=" "$env_file"; then
    echo "kept      $1"
  else
    printf '%s=%s\n' "$1" "$2" >> "$env_file"
    echo "generated $1"
  fi
}

# Hex: letters and digits only, safe inside URLs and JSON
rand() { openssl rand -hex "$1"; }

add_var POSTGRES_USER     musicbox
add_var POSTGRES_DB       musicbox
add_var POSTGRES_PASSWORD "$(rand 24)"
add_var DATABASE_URL      'postgres://musicbox:${POSTGRES_PASSWORD}@127.0.0.1:5432/musicbox?sslmode=disable'
add_var S3_ENDPOINT       http://127.0.0.1:8333
add_var S3_BUCKET         musicbox
add_var S3_ACCESS_KEY     "$(rand 10)"
add_var S3_SECRET_KEY     "$(rand 20)"

# s3.json is derived from .env, so it's rewritten on every run
set -a; . "./$env_file"; set +a
mkdir -p seaweedfs
cat > "$s3_config" <<EOF
{
  "identities": [
    {
      "name": "musicbox-api",
      "credentials": [{ "accessKey": "$S3_ACCESS_KEY", "secretKey": "$S3_SECRET_KEY" }],
      "actions": ["Read:$S3_BUCKET", "Write:$S3_BUCKET", "List:$S3_BUCKET"]
    }
  ]
}
EOF
chmod 600 "$env_file" "$s3_config"
echo "wrote     $s3_config"
