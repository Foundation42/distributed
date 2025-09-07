#!/bin/sh
# Test script for llama.cpp server

echo "Testing llama.cpp server health endpoint..."
curl -s http://localhost:8080/health | jq .

echo -e "\n\nTesting model info..."
curl -s http://localhost:8080/v1/models | jq .

echo -e "\n\nTesting completion endpoint..."
curl -s -X POST http://localhost:8080/v1/completions \
  -H "Content-Type: application/json" \
  -d '{
    "prompt": "Hello, I am",
    "max_tokens": 50,
    "temperature": 0.7
  }' | jq .

echo -e "\n\nTesting chat completion endpoint..."
curl -s -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "What is 2+2?"}
    ],
    "max_tokens": 50,
    "temperature": 0.7
  }' | jq .