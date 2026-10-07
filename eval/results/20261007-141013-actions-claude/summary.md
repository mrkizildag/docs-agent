# Eval summary

Actions runner, models `claude-sonnet-5-5`, judge `claude`, 3 runs per case.

| Case | Expect | Pass | Recall | Precision | Fact coverage | Errored | Failures |
|---|---|---|---|---|---|---|---|
| apply-skip-new-doc | proposals | 1/3 | 0.67 | 0.67 | 0.50 | 1 | error, facts missing |
| doc-link-source-files | no_impact | 3/3 | - | - | - | 0 |  |
| docker-deploy-new-doc | proposals | 0/3 | 0.67 | 0.67 | 0.17 | 1 | error, facts missing |
| escape-model-text | proposals | 1/3 | 0.67 | 0.67 | 0.56 | 1 | error, facts missing |
| evict-idle-buckets | no_impact | 3/3 | - | - | - | 0 |  |
| import-boundaries | proposals | 0/3 | 0.00 | 1.00 | 0.00 | 0 | wrong verdict |
| manual-deploy | proposals | 0/3 | 0.00 | 0.00 | 0.00 | 3 | error |
| pin-new-doc-covers-tests | no_impact | 3/3 | - | - | - | 0 |  |
| pin-skip-reason-tests | no_impact | 3/3 | - | - | - | 0 |  |
| prune-jobs | proposals | 0/3 | 0.00 | 0.00 | 0.00 | 3 | error |
| reject-empty-actions-result | no_impact | 0/3 | - | - | - | 0 | wrong verdict |
| scaffold-adopt-drift | proposals | 3/3 | 1.00 | 1.00 | 1.00 | 0 |  |
| scaffold-gives-up-test | no_impact | 3/3 | - | - | - | 0 |  |
| webhook-rate-limit | proposals | 0/3 | 0.00 | 0.00 | 0.00 | 3 | error |

## Totals

Deltas are against 20261007-100338-actions-claude.

- Overall pass rate: 47.6% (-2.4 pp)
- Impact-case pass rate: 20.8% (-4.2 pp)
- No-impact accuracy: 83.3% (+0.0 pp)
- Mean fact coverage: 27.8% (-0.3 pp)
