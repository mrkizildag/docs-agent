# Eval summary

Actions runner, models `claude-sonnet-5-5`, judge `claude`, 1 runs per case.

| Case | Expect | Pass | Recall | Precision | Fact coverage | Errored | Failures |
|---|---|---|---|---|---|---|---|
| apply-skip-new-doc | proposals | 1/1 | 1.00 | 1.00 | 1.00 | 0 |  |
| doc-link-source-files | no_impact | 1/1 | - | - | - | 0 |  |
| docker-deploy-new-doc | proposals | 0/1 | 1.00 | 1.00 | 0.25 | 0 | facts missing |
| escape-model-text | proposals | 0/1 | 0.00 | 0.00 | 0.00 | 1 | error |
| evict-idle-buckets | no_impact | 1/1 | - | - | - | 0 |  |
| import-boundaries | proposals | 0/1 | 0.00 | 1.00 | 0.00 | 0 | wrong verdict |
| manual-deploy | proposals | 0/1 | 0.00 | 0.00 | 0.00 | 1 | error |
| pin-new-doc-covers-tests | no_impact | 1/1 | - | - | - | 0 |  |
| pin-skip-reason-tests | no_impact | 1/1 | - | - | - | 0 |  |
| prune-jobs | proposals | 0/1 | 0.00 | 0.00 | 0.00 | 1 | error |
| reject-empty-actions-result | no_impact | 0/1 | - | - | - | 0 | wrong verdict |
| scaffold-adopt-drift | proposals | 1/1 | 1.00 | 1.00 | 1.00 | 0 |  |
| scaffold-gives-up-test | no_impact | 1/1 | - | - | - | 0 |  |
| webhook-rate-limit | proposals | 0/1 | 0.00 | 0.00 | 0.00 | 1 | error |

## Totals

- Overall pass rate: 50.0%
- Impact-case pass rate: 25.0%
- No-impact accuracy: 83.3%
- Mean fact coverage: 28.1%
