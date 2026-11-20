#!/usr/bin/env bash
# claude-clipboard-probe.sh - check that the installed Claude Code still uses
# the clipboard behaviour yolobox's clipboard support relies on.
#
#   bash scripts/claude-clipboard-probe.sh [path-to-claude]
#
# yolobox answers Claude's clipboard commands with shims in the box
# (cmd/yolobox/assets/yolobox-clipboard) and keeps Claude in WSL mode on Windows
# hosts. That depends on Claude internals, read from its binary:
#
#   - the image check and save commands (xclip / wl-paste with MIME types)
#   - Alt+V as the image-paste key on Windows and WSL
#   - the WSL-mode PowerShell scripts for copy and text read
#   - the OSC 52 copy fallback
#
# Exit status: 0 all present; 1 something changed (update the shim and
# clipboard_shim_test.go, then this script); 2 Claude not found.
#
# The literal strings below must match the constants in
# cmd/yolobox/clipboard_shim_test.go; a Go test checks that they do.

set -euo pipefail

find_claude() {
    local candidate resolved
    while read -r candidate; do
        resolved="$(readlink -f "$candidate")"
        # Skip yolobox's own wrapper, which execs the real binary.
        case "$resolved" in /opt/yolobox/*) continue ;; esac
        printf '%s' "$resolved"
        return 0
    done < <(type -ap claude)
    return 1
}

if [ "$#" -gt 0 ]; then
    claude_bin="$(readlink -f "$1")"
else
    claude_bin="$(find_claude)" || { echo "claude not found on PATH; pass its path" >&2; exit 2; }
fi
[ -f "$claude_bin" ] || { echo "not a file: $claude_bin" >&2; exit 2; }

version="$("$claude_bin" --version 2>/dev/null | head -1 || true)"
echo "Claude Code: ${version:-unknown} ($claude_bin)"

perl -e '
use strict;
use warnings;

my $file = shift;
open my $fh, "<:raw", $file or die "cannot read $file: $!\n";
my $data = do { local $/; <$fh> };

# A placeholder for a minified JavaScript template variable, e.g. ${r}.
my $var = q(\$\{\w+\});

my @checks = (
    ["image check command (xclip/wl-paste TARGETS)", "fixed",
     q{xclip -selection clipboard -t TARGETS -o 2>/dev/null | grep -E "image/(png|jpeg|jpg|gif|webp|bmp)" || wl-paste -l 2>/dev/null | grep -E "image/(png|jpeg|jpg|gif|webp|bmp)"}],
    ["image save command (image/png, then image/bmp)", "regex",
     join($var, map { quotemeta } (
        "xclip -selection clipboard -t image/png -o > ", " 2>/dev/null || wl-paste --type image/png > ",
        " 2>/dev/null || xclip -selection clipboard -t image/bmp -o > ", " 2>/dev/null || wl-paste --type image/bmp > ", ""))],
    ["Alt+V image paste on Windows and WSL", "regex",
     q{==="windows"\|\|\w+==="wsl",\w+=\w+\?"alt\+v":"ctrl\+v"}],
    ["WSL copy script (Set-Clipboard)", "fixed",
     q{[Console]::InputEncoding = [Text.Encoding]::UTF8; Set-Clipboard -Value ([Console]::In.ReadToEnd())}],
    ["WSL text read script (Get-Clipboard)", "fixed",
     q{[Console]::OutputEncoding = [Text.Encoding]::UTF8; Get-Clipboard -Raw}],
    ["OSC 52 copy sequence", "fixed", q{]52;c;}],
);

my $failed = 0;
for my $check (@checks) {
    my ($name, $kind, $pattern) = @$check;
    my $found = $kind eq "fixed" ? index($data, $pattern) >= 0 : $data =~ /$pattern/;
    printf "  %s %s\n", ($found ? "ok  " : "FAIL"), $name;
    $failed = 1 unless $found;
}
exit $failed;
' "$claude_bin"
