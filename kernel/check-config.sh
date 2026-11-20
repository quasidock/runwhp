#!/bin/sh
# check-config.sh FRAGMENT CONFIG
#
# Fails, listing them, if settings in FRAGMENT did not make it into CONFIG.
# Kconfig drops a requested setting without an error when its dependencies
# are not met, or when upstream renamed or removed the symbol. A
# "# CONFIG_X is not set" line only fails if CONFIG_X ended up enabled.
set -eu

fragment=$1
config=$2
missing=$(
	grep -E '^(CONFIG_[A-Za-z0-9_]+=|# CONFIG_[A-Za-z0-9_]+ is not set$)' "$fragment" |
		while IFS= read -r line; do
			case $line in
			"# "*)
				symbol=${line#\# }
				symbol=${symbol%% *}
				if grep -qE "^$symbol=" "$config"; then
					printf '  %s, but %s\n' "$line" "$(grep -E "^$symbol=" "$config")"
				fi
				;;
			*)
				grep -qxF -- "$line" "$config" || printf '  %s\n' "$line"
				;;
			esac
		done
)
if [ -n "$missing" ]; then
	echo "check-config.sh: these settings from $(basename "$fragment") are not in the configuration:" >&2
	echo "$missing" >&2
	exit 1
fi
