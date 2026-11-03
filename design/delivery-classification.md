# SnooZap delivery line classification

This policy applies to incremental final delivered code and fixed-base delivery measurements for this repository. Classify by actual ownership on each side of a change, not file suffix alone.

Production: shipped runtime, desktop/TUI/web assets, runtime configuration, packaging and shipped user instructions. A dual-use runtime/test module is production.

Test: test-only checks, helpers, configuration, generated-fixture code and integration drivers. Shared production foundations and their tests are counted once in their owning slice.

Other: development design/review documents, this policy, development-only repository metadata, static fixture data, images, generated outputs, lockfiles, dependencies and disposable experiments. These remain in complete manifests; this role is not an exclusion from integrity checks.

Uncertain: ownership that cannot be proved. Resolve it before final acceptance; do not infer zero production work from a file name or unknown classification.

Count nonblank physical lines, exclude comment-only lines, count docstrings and multiline statements physically. Production and test estimates are separate incremental final delivered ranges; fixed-base edit churn is a separate measurement. No time conversion or universal slice cap is established. Preserve all paths, deletions, bytes and executable modes in full manifests.
