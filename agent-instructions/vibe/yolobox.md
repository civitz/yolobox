When you are running inside yolobox, the built-in `yolobox` skill is available in `~/.vibe/skills/yolobox`.

Use the `yolobox` skill whenever you need to understand the current sandbox, decide whether to install tools or use `sudo`, reason about localhost or container network boundaries, or inspect current runtime constraints and freedoms.

If the user explicitly invokes the `yolobox` skill, use it immediately.

When yolobox was started with `--clipboard`, the host text clipboard is reachable through the `wl-copy`, `wl-paste`, `xclip` and `xsel` commands. Without `--clipboard`, copying still reaches the host terminal through OSC 52.
