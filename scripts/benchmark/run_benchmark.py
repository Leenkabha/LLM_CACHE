"""Measured HTTP benchmark. Produces no claimed results until services run."""
import argparse, csv, hashlib, html, json, math, statistics, subprocess, time
from datetime import datetime, timezone
from pathlib import Path
from urllib.request import Request, urlopen
from urllib.error import HTTPError, URLError

def request(url, body=None, timeout=180):
    data=None if body is None else json.dumps(body).encode()
    req=Request(url,data=data,headers={'Content-Type':'application/json'})
    start=time.perf_counter()
    try:
        with urlopen(req,timeout=timeout) as r:
            result=json.loads(r.read())
        return result,(time.perf_counter()-start)*1000
    except HTTPError as e:
        # Provider errors may contain URLs with credentials: never persist body.
        raise RuntimeError('HTTP '+str(e.code)) from None
    except (URLError,TimeoutError):
        raise RuntimeError('connection failure or timeout') from None

def settled(url, expected, timeout=30):
    start=time.perf_counter()
    while time.perf_counter()-start<timeout:
        stats,_=request(url+'/stats',timeout=10)
        if stats['size']>=expected:
            return (time.perf_counter()-start)*1000
        time.sleep(.05)
    raise RuntimeError('async cache update did not settle')

def percentile(values, q):
    vals=sorted(values); i=(len(vals)-1)*q; lo=math.floor(i); hi=math.ceil(i)
    return vals[lo]+(vals[hi]-vals[lo])*(i-lo)

def summary(rows):
    result={}
    for arm in ('no-cache','cache'):
        group=[r for r in rows if r['arm']==arm]
        ok=[r for r in group if r['status']=='ok']
        values=[r['client_ms'] for r in ok]
        result[arm]={'requests':len(group),'successes':len(ok),'errors':len(group)-len(ok),
            'hits':sum(r['cache_hit'] for r in ok),'successful_llm_completions':sum(not r['cache_hit'] for r in ok),
            'sum_response_ms':sum(values), 'mean_ms':statistics.mean(values) if values else None,
            'median_ms':statistics.median(values) if values else None,'p95_ms':percentile(values,.95) if values else None}
        result[arm]['hit_rate']=result[arm]['hits']/len(ok) if ok else None
    base,cached=result['no-cache'],result['cache']
    if base['errors']==cached['errors']==0 and base['requests']==cached['requests'] and base['sum_response_ms']>0 and cached['sum_response_ms']>0:
        result['comparison']={'latency_reduction_percent':100*(1-cached['sum_response_ms']/base['sum_response_ms']),
          'response_time_speedup':base['sum_response_ms']/cached['sum_response_ms'],
          'avoided_successful_completions':base['successful_llm_completions']-cached['successful_llm_completions']}
    return result

def main():
    ap=argparse.ArgumentParser()
    ap.add_argument('--cache-url',default='http://127.0.0.1:18080')
    ap.add_argument('--baseline-url',default='http://127.0.0.1:18081')
    ap.add_argument('--workload',type=Path,default=Path(__file__).with_name('workload.json'))
    ap.add_argument('--out',type=Path,required=True)
    ap.add_argument('--trials',type=int,default=2)
    ap.add_argument('--allow-isolated-flush',action='store_true')
    ap.add_argument('--llm-pause',type=float,default=0,help='seconds to wait after each real LLM call (outside timing) to respect provider rate limits')
    args=ap.parse_args()
    if not args.allow_isolated_flush: ap.error('Use only a dedicated benchmark instance; --allow-isolated-flush is required.')
    if args.trials<1: ap.error('trials must be positive')
    spec=json.loads(args.workload.read_text(encoding='utf-8-sig')); queries=spec['queries']
    if not queries or len({q['id'] for q in queries})!=len(queries): ap.error('nonempty workload with unique IDs required')
    args.out.mkdir(parents=True,exist_ok=False)
    meta={'started_utc':datetime.now(timezone.utc).isoformat(),'workload':spec,'trials':args.trials,
          'llm_pause_s':args.llm_pause,'workload_sha256':hashlib.sha256(args.workload.read_bytes()).hexdigest(),
          'method':'Sequential HTTP requests. Identical order in both arms. Empty cache at each trial start. Alternating arm order. Async settlement waits excluded from response latency but recorded separately.',
          'limitations':'Successful logical LLM completions, not provider retries, tokens or billed cost. Small authored workload unless replaced. Response quality needs manual review.'}
    rows=[]
    repo=Path(__file__).resolve().parents[2]
    try:
        meta['source_commit']=subprocess.check_output(['git','rev-parse','HEAD'],cwd=repo,text=True,stderr=subprocess.DEVNULL,timeout=10).strip()
        meta['source_dirty']=bool(subprocess.check_output(['git','status','--porcelain','--untracked-files=no'],cwd=repo,text=True,stderr=subprocess.DEVNULL,timeout=10).strip())
    except (OSError,subprocess.SubprocessError):
        meta['source_commit']='unavailable (record revision before sharing results)'
    meta['cache_settings']={'threshold':.25,'top_k':1,'capacity':1000,'policy':'lru','embedding':'all-MiniLM-L6-v2','metric':'cosine','origin':'supplied compose.yml; do not override without updating this record'}
    try:
        meta['baseline_health'],_=request(args.baseline_url+'/health')
        meta['cache_health'],_=request(args.cache_url+'/health')
        if meta['baseline_health'].get('provider') not in ('openai','gemini'): raise RuntimeError('Real-provider benchmark requires openai or gemini; simulation must be reported separately.')
        # Warm both paths and the embedding model; these calls are outside samples.
        warm={'prompt':'What is a computer? Answer in one short sentence.'}
        request(args.baseline_url+'/query',warm); time.sleep(args.llm_pause)
        request(args.cache_url+'/flush',{})
        request(args.cache_url+'/query',warm); time.sleep(args.llm_pause)
        settled(args.cache_url,1)
        meta['warmup_calls']=2
        for trial in range(1,args.trials+1):
            order=('no-cache','cache') if trial%2 else ('cache','no-cache')
            for arm in order:
                endpoint=args.baseline_url if arm=='no-cache' else args.cache_url
                expected_size=0
                if arm=='cache': request(endpoint+'/flush',{})
                for q in queries:
                    row={'trial':trial,'arm':arm,**q,'utc':datetime.now(timezone.utc).isoformat()}
                    try:
                        response,ms=request(endpoint+'/query',{'prompt':q['prompt']})
                        row.update(status='ok',client_ms=ms,server_ms=response.get('latency_ms'),cache_hit=response.get('cache_hit',False),response=response,settlement_wait_ms=0)
                        if arm=='cache' and not row['cache_hit']:
                            expected_size+=1
                            row['settlement_wait_ms']=settled(endpoint,expected_size)
                    except RuntimeError as e:
                        row.update(status='error',error=str(e))
                    rows.append(row)
                    if row.get('status')=='ok' and not row['cache_hit']: time.sleep(args.llm_pause)
                    with (args.out/'requests.jsonl').open('a',encoding='utf8') as f: f.write(json.dumps(row,ensure_ascii=False)+'\n')
                    print(json.dumps({k:row[k] for k in ('trial','arm','id','status','client_ms','cache_hit') if k in row}),flush=True)
                    if row['status']!='ok': raise RuntimeError('Run stopped after request failure; partial data retained, no savings claim.')
                if arm=='cache':
                    meta.setdefault('cache_stats',[]).append({'trial':trial,'stats':request(endpoint+'/stats')[0]})
        meta['status']='complete'
    except Exception as e:
        meta['status']='incomplete';meta['error']=str(e) if isinstance(e,RuntimeError) else type(e).__name__
    finally:
        meta['ended_utc']=datetime.now(timezone.utc).isoformat()
        meta['summary']=summary(rows)
        if meta['status']!='complete': meta['summary'].pop('comparison',None)
        (args.out/'run.json').write_text(json.dumps(meta,indent=2,ensure_ascii=False),encoding='utf8')
        if rows:
            with (args.out/'requests.csv').open('w',newline='',encoding='utf8') as f:
                fields=['trial','arm','id','kind','status','client_ms','server_ms','cache_hit','settlement_wait_ms']
                writer=csv.DictWriter(f,fieldnames=fields,extrasaction='ignore');writer.writeheader();writer.writerows(rows)
        # This is a transparent view of measured records, not a simulated terminal.
        body=html.escape(json.dumps(meta,indent=2,ensure_ascii=False))
        (args.out/'evidence.html').write_text('<!doctype html><meta charset="utf-8"><title>Benchmark evidence</title><style>body{font:18px monospace;background:#102b3b;color:#e7f3ef;padding:40px}pre{white-space:pre-wrap}</style><h1>Measured benchmark evidence</h1><pre>'+body+'</pre>',encoding='utf8')
    if meta['status']!='complete': raise SystemExit('Benchmark incomplete: '+meta['error'])

if __name__=='__main__': main()
