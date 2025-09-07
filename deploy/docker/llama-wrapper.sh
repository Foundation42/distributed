#!/bin/sh
# Wrapper script for llama-server to handle dynamic model loading

# Default model path
MODEL_DIR="/models"

# Find the first .gguf file in the models directory
FIRST_MODEL=$(find "$MODEL_DIR" -name "*.gguf" -type f | head -n 1)

if [ -z "$FIRST_MODEL" ]; then
    echo "No GGUF models found in $MODEL_DIR"
    echo "Server will exit. Please mount a volume with at least one .gguf model file."
    exit 1
fi

echo "Found model: $FIRST_MODEL"
echo "Starting llama-server with initial model..."

# Start the server with the first available model
exec /usr/local/bin/llama-server \
    --model "$FIRST_MODEL" \
    "$@"