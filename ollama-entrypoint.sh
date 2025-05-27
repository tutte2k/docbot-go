#!/bin/sh
ollama serve &
sleep 5 
ollama pull smollm2:1.7b
wait