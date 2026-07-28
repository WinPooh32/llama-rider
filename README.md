# llama-rider

Proof of concept project for controlling llama.cpp KV-cache disk offload using http API endpoints.

## Cache Management

The project uses a **disk-based KV-cache** for llama.cpp, implemented as an HTTP proxy in front of `llama-server`. The cache persists the model's KV-slot state to disk so conversations can be resumed without re-running the full prompt through the model (cache prefill).

**Note:** this only works in single-threaded mode — only one conversation slot is managed at a time.

### What is cached

Two types of cache files are stored in the `slot-save-path` directory:

| File | Purpose |
| --- | --- |
| `<model>--chat.bin` | Chat history for a specific model |
| `<model>--system--<hash>.bin` | System prompt for a specific model (hash derived from prompt + tools + instruction messages) |

### When things happen (on each new request)

1. **System cache not found** (new/unfamiliar system prompt):
   - Save the current model cache (if any)
   - Erase the slot in llama-server
   - Send a "warmup" request — run the system prompt through the model
   - Save the result as the system cache

2. **Model switched, but conversation has replies** (continuing a chat):
   - Save the current model cache
   - Restore the chat cache for the new model

3. **Model switched, conversation is empty** (new chat):
   - Save the current model cache
   - Restore the system cache (pre-warmed earlier)

4. **Same model, same conversation** — nothing to do.

### On shutdown

The model cache is saved one final time to prevent state loss.

### What is NOT implemented

- **No LRU** — old files are never evicted, they accumulate on disk.
- **No TTL** — files do not expire.
- **No size-based policy** — decisions are based solely on file existence on disk.

In short: **"save before switching, restore if it was already there"** — every time.
