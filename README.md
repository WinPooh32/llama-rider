# llama-rider

Proof of concept project for controlling llama.cpp KV-cache disk offload using http API endpoints.

## Installation

```sh
go install github.com/WinPooh32/llama-rider@latest
```

## Cache Management

The project uses a **disk-based KV-cache** for llama.cpp, implemented as an HTTP proxy in front of `llama-server`. The cache persists the model's KV-slot state to disk so conversations can be resumed without re-running the full prompt through the model (cache prefill).

**Note:** this only works in single-threaded mode — only one conversation slot is managed at a time.

### Usage

```sh
llama-rider -port <port> -system-cache-limit <n> /path/to/llama-server [llama-server args...]
```

| Flag | Default | Description |
| --- | --- | --- |
| `-port` | `"8080"` | Proxy listen port |
| `-system-cache-limit` | `0` | Max system caches per model (`0` = unlimited) |

### llama-server required flags

The proxy extracts the following flags from llama-server arguments. They **must** be present:

| Flag | Description |
| --- | --- |
| `--port` | Port llama-server listens on (proxy forwards to it) |
| `--alias` | Model alias (used as prefix for cache filenames) |
| `--slot-save-path` | Directory where `.bin` cache files are stored |

### llama-server recommended settings

I found these settings optimal for the 130k tokens context window:

```sh
--cache-ram 4096
--ctx-checkpoints 32
--checkpoint-min-step 4096
-ctk q8_0
-ctv q8_0
```

Read the [llama-server README](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md) for more details.

Example disk usage for multiple models switched via llama-swap:

```sh
$ du -h
6,1G	.
```

```sh
$ du -sh *
1,2G	qwen3.6-27b--chat.bin
1,1G	qwen3.6-27b--chat.bin.ckpt
161M	qwen3.6-27b--system--62f45e3f.bin
300M	qwen3.6-27b--system--62f45e3f.bin.ckpt
567M	qwen3.6-27b--system--a1f7fca1.bin
449M	qwen3.6-27b--system--a1f7fca1.bin.ckpt
565M	qwen3.6-27b--system--f1272f16.bin
449M	qwen3.6-27b--system--f1272f16.bin.ckpt
264M	qwen3.6-35b-a3b--agent--chat.bin
315M	qwen3.6-35b-a3b--agent--chat.bin.ckpt
141M	qwen3.6-35b-a3b--agent--system--8a2de77a.bin
126M	qwen3.6-35b-a3b--agent--system--8a2de77a.bin.ckpt
138M	qwen3.6-35b-a3b--agent--system--bdffaecb.bin
126M	qwen3.6-35b-a3b--agent--system--bdffaecb.bin.ckpt
197M	qwen3.6-35b-a3b--agent--system--f94279e4.bin
189M	qwen3.6-35b-a3b--agent--system--f94279e4.bin.ckpt
```

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

### Cache Cleanup

By default, system cache files accumulate on disk. To limit their number, use the `-system-cache-limit` flag:

```
llama-rider -system-cache-limit 5 /path/to/llama-server [llama-server args...]
```

This keeps at most `N` system caches per model. When a new cache is saved and the limit is exceeded, the oldest caches (by modification time) are automatically removed along with their `.ckpt` sidecar files.

- **Default (`0`)**: unlimited — caches are never evicted.
- Chat caches (`<model>--chat.bin`) are not affected by this limit.

### What is NOT implemented

- **No TTL** — files do not expire.
- **No size-based policy** — decisions are based solely on file existence on disk.

In short: **"save before switching, restore if it was already there"** — every time.
