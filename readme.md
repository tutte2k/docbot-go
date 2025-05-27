docker-compose --build
kan behövas vänta på att model laddas ner för ollama appen

när det klart
go run main.go

git clone https://github.com/ggerganov/llama.cpp
cd llama.cpp
wsl
sudo apt install cmake
cmake -B build -DLLAMA_CURL=OFF
cmake --build build
