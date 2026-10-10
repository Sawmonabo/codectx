# product-topk vs product per query (30-query set, 5 warm runs, same binary, run back to back)

| query | hits | product warm median ms | product-topk warm median ms | ratio | page identical | blocks scored / total (skipped) | documents scored |
|---|---|---|---|---|---|---|---|
| vendor | 252683 | 876.56 | 8.69 | 0.01x | yes | 225/4159 (94.6%) | 10131 |
| func | 199866 | 679.73 | 22.10 | 0.03x | yes | 1825/4029 (54.7%) | 97817 |
| src | 167573 | 514.60 | 10.06 | 0.02x | yes | 438/3859 (88.6%) | 19962 |
| staging | 166345 | 499.75 | 8.90 | 0.02x | yes | 329/3772 (91.3%) | 18618 |
| org | 166465 | 492.35 | 14.39 | 0.03x | yes | 646/3902 (83.4%) | 21014 |
| output | 8900 | 91.17 | 10.68 | 0.12x | yes | 338/2174 (84.5%) | 1499 |
| metrics | 9440 | 105.48 | 11.29 | 0.11x | yes | 553/2245 (75.4%) | 3478 |
| net | 9731 | 104.42 | 8.60 | 0.08x | yes | 326/2171 (85.0%) | 1111 |
| service | 8745 | 99.77 | 10.16 | 0.10x | yes | 352/2430 (85.5%) | 1892 |
| meta | 8601 | 87.21 | 7.50 | 0.09x | yes | 161/1429 (88.7%) | 694 |
| actions | 834 | 16.85 | 10.14 | 0.60x | yes | 218/412 (47.1%) | 499 |
| progress | 872 | 18.59 | 11.72 | 0.63x | yes | 324/522 (37.9%) | 513 |
| tracker | 975 | 19.70 | 10.10 | 0.51x | yes | 229/534 (57.1%) | 507 |
| encrypt | 980 | 18.24 | 8.84 | 0.48x | yes | 268/474 (43.5%) | 652 |
| indicate | 857 | 18.52 | 12.70 | 0.69x | yes | 375/485 (22.7%) | 469 |
| comparelock | 2 | 0.47 | 0.33 | 0.71x | yes | 2/2 (0.0%) | 2 |
| currentutilizationval | 2 | 0.46 | 0.29 | 0.64x | yes | 4/4 (0.0%) | 4 |
| messageoptionsr | 2 | 0.44 | 0.33 | 0.75x | yes | 2/2 (0.0%) | 2 |
| cachedresource | 5 | 0.53 | 0.40 | 0.77x | yes | 7/7 (0.0%) | 7 |
| defaultstubrefreshtoken | 4 | 0.57 | 0.43 | 0.75x | yes | 6/6 (0.0%) | 6 |
| pod scheduler | 34633 | 176.29 | 14.43 | 0.08x | yes | 1503/3570 (57.9%) | 19683 |
| resource version | 45634 | 211.85 | 19.64 | 0.09x | yes | 2338/3811 (38.7%) | 38749 |
| node status | 28526 | 176.55 | 18.27 | 0.10x | yes | 2233/3740 (40.3%) | 24371 |
| client config | 63616 | 257.65 | 17.11 | 0.07x | yes | 1857/3488 (46.8%) | 45500 |
| volume mount | 13779 | 128.87 | 12.58 | 0.10x | yes | 842/2847 (70.4%) | 6357 |
| "resource version" | 421 | 9.03 | 8.47 | 0.94x | yes | not printed (phrase) | — |
| "garbage collector" | 300 | 8.09 | 7.47 | 0.92x | yes | not printed (phrase) | — |
| "service account" | 784 | 17.63 | 10.53 | 0.60x | yes | not printed (phrase) | — |
| "node name" | 784 | 16.42 | 12.76 | 0.78x | yes | not printed (phrase) | — |
| "admission controller" | 261 | 5.63 | 7.42 | 1.32x | yes | not printed (phrase) | — |
