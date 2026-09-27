#!/bin/sh
# A witness that attests exactly one head: the vectors of
# evidence-schema.md §9.2.5, driven by the environment rather than by
# argv, because argv is the contract under test.
#
#   PROBAVI_TEST_HEAD   the head this witness holds an attestation for
#   PROBAVI_TEST_AT     what to print on stdout when it attests
#   PROBAVI_TEST_QUIET  set to 1 to refuse without a diagnostic
#
# Exit 0 attested, 1 not attested. It never exits anything else: that is
# witness-fail.sh's job.
set -u
if [ "${1:-}" != "${PROBAVI_TEST_HEAD:-}" ]; then
    [ "${PROBAVI_TEST_QUIET:-}" = "1" ] || echo "no attestation for ${1:-<none>}" >&2
    exit 1
fi
[ -z "${PROBAVI_TEST_AT:-}" ] || echo "$PROBAVI_TEST_AT"
exit 0
