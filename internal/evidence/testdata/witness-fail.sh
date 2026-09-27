#!/bin/sh
# A witness that cannot answer — §9.2.2's third row. Exit 3 stands for
# every status that is not 0 or 1: a crash, a missing credential, an
# authority that did not respond. None of them says anything about the
# log.
#
#   PROBAVI_TEST_QUIET  set to 1 to fail without a diagnostic
set -u
[ "${PROBAVI_TEST_QUIET:-}" = "1" ] || echo "timestamp authority unreachable" >&2
exit 3
