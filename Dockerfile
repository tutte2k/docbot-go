FROM golang:1.23.9

# Install dependencies for llama-embedding
RUN apt-get update && apt-get install -y \
    build-essential \
    libopenblas-dev \
    && rm -rf /var/lib/apt/lists/*

# Copy the Go application
WORKDIR /app
COPY . .

# Copy the prebuilt llama-embedding binaries
COPY ./llama.cpp/build/bin/llama-embedding /app/llama-embedding
COPY ./llama.cpp/build/bin/libllama.so /app/libllama.so
COPY ./llama.cpp/build/bin/libggml.so /app/libggml.so
COPY ./llama.cpp/build/bin/libggml-base.so /app/libggml-base.so
COPY ./llama.cpp/build/bin/libggml-cpu.so /app/libggml-cpu.so

# Copy model file
COPY ./models/nomic-embed-text-v1.5.Q4_K_M.gguf /app/models/nomic-embed-text-v1.5.Q4_K_M.gguf

# Verify files and set permissions
RUN ls -l /app/llama-embedding /app/libllama.so /app/libggml.so /app/libggml-base.so /app/libggml-cpu.so && \
    chmod +x /app/llama-embedding && \
    chmod +r /app/libllama.so /app/libggml.so /app/libggml-base.so /app/libggml-cpu.so

ENV LD_LIBRARY_PATH=/app
ENV LD_LIBRARY_PATH=/app:$LD_LIBRARY_PATH

# Build the Go application
RUN go build -o docbot .

# Expose the port
EXPOSE 8080

# Run the application
CMD ["./docbot"]