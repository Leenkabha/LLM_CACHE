"""Mathematical checks only; these are not performance measurements."""
from run_benchmark import summary, percentile
assert percentile([10,20,30,40],.95)==38.5
rows=[{'arm':'no-cache','status':'ok','client_ms':100,'cache_hit':False},
      {'arm':'no-cache','status':'ok','client_ms':100,'cache_hit':False},
      {'arm':'cache','status':'ok','client_ms':110,'cache_hit':False},
      {'arm':'cache','status':'ok','client_ms':10,'cache_hit':True}]
s=summary(rows)
assert s['comparison']['latency_reduction_percent']==40
assert s['comparison']['avoided_successful_completions']==1
assert s['cache']['hit_rate']==.5
rows[3]={'arm':'cache','status':'error'}
assert 'comparison' not in summary(rows)
assert summary([])['cache']['mean_ms'] is None
print('Metric checks passed. No performance benchmark executed.')
