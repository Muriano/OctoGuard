#!/bin/sh
# Install the octoguard binary and its agent skill.
#
#   curl -fsSL https://raw.githubusercontent.com/Muriano/OctoGuard/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/Muriano/OctoGuard/main/install.sh | sh -s -- --agent claude --scope user
#
# Options (passed to `octoguard skills install`):
#   --agent claude|codex|gemini|copilot|all   (default: all)
#   --scope user|project                      (default: user)
#
# Environment:
#   OCTOGUARD_BIN_DIR  where to put the binary when Homebrew is not used
#                      (default: $HOME/.local/bin)
set -eu

repo="Muriano/OctoGuard"
agent="all"
scope="user"

while [ $# -gt 0 ]; do
	case "$1" in
	--agent) agent="${2:?--agent needs a value}"; shift 2 ;;
	--scope) scope="${2:?--scope needs a value}"; shift 2 ;;
	-h | --help) echo "usage: install.sh [--agent claude|codex|gemini|copilot|all] [--scope user|project]"; exit 0 ;;
	*) echo "install.sh: unknown option $1" >&2; exit 2 ;;
	esac
done

say() { echo "octoguard-install: $*" >&2; }

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

install_from_release() {
	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) say "unsupported OS $(uname -s)"; exit 1 ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) say "unsupported architecture $(uname -m)"; exit 1 ;;
	esac

	# Resolve the latest tag from the release redirect: no GitHub API quota used.
	tag=$(curl -fsSIL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")
	tag=${tag##*/}
	case "$tag" in
	v[0-9]*) ;;
	*) say "could not resolve the latest release (got '$tag')"; exit 1 ;;
	esac
	version=${tag#v}

	archive="octoguard_${version}_${os}_${arch}.tar.gz"
	base="https://github.com/$repo/releases/download/$tag"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	say "downloading $archive"
	curl -fsSL -o "$tmp/$archive" "$base/$archive"
	curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

	want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
	got=$(sha256 "$tmp/$archive")
	if [ -z "$want" ] || [ "$want" != "$got" ]; then
		say "checksum mismatch for $archive"
		exit 1
	fi

	bin_dir="${OCTOGUARD_BIN_DIR:-$HOME/.local/bin}"
	mkdir -p "$bin_dir"
	tar -xzf "$tmp/$archive" -C "$tmp" octoguard
	install -m 0755 "$tmp/octoguard" "$bin_dir/octoguard"
	say "installed $bin_dir/octoguard"
	case ":$PATH:" in
	*":$bin_dir:"*) ;;
	*)
		say "$bin_dir is not in PATH; add it to your shell profile"
		PATH="$bin_dir:$PATH"
		;;
	esac
}

if command -v octoguard >/dev/null 2>&1; then
	say "octoguard already installed: $(command -v octoguard)"
elif command -v brew >/dev/null 2>&1; then
	say "installing with Homebrew"
	brew install muriano/tap/octoguard
else
	install_from_release
fi

octoguard --version
octoguard skills install --agent "$agent" --scope "$scope" --format text
