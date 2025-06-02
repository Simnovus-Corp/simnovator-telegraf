#!/bin/bash

set -e

# Define tag list
TAGS="custom,inputs.websocket,inputs.kafka_consumer,outputs.kafka,outputs.elasticsearch,outputs.postgresql,outputs.websocket,outputs.file,parsers.xpath,parsers.json,parsers.json_v2,parsers.influx,processors.date,processors.timestamp,serializers.json"

# Image and container names
IMAGE_NAME="my-telegraf-builder"
CONTAINER_NAME="temp-telegraf"

echo "🛠️  Building Telegraf binary inside Docker..."
docker build \
  -f dockerfile.build \
  -t "$IMAGE_NAME" \
  --build-arg TAGS="$TAGS" .

echo "📦  Creating temporary container..."
docker create --name "$CONTAINER_NAME" "$IMAGE_NAME"

echo "📤  Copying Telegraf binary to host..."
docker cp "$CONTAINER_NAME":/telegraf/telegraf ./telegraf

echo "🧹  Cleaning up..."
docker rm "$CONTAINER_NAME"

echo "✅  Done! Telegraf binary is available at: ./telegraf"
