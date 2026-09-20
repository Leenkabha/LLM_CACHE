"""Plot recorded COMPLETE runs only. Never creates synthetic benchmark data."""
import argparse,json
from pathlib import Path
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt

ap=argparse.ArgumentParser();ap.add_argument('run',type=Path);args=ap.parse_args()
record=json.loads((args.run/'run.json').read_text(encoding='utf8'))
if record.get('status')!='complete': raise SystemExit('Refusing to plot an incomplete run.')
stats=record['summary']
if 'comparison' not in stats: raise SystemExit('No valid paired comparison.')
plt.rcParams.update({'font.family':'DejaVu Sans','font.size':14,'axes.spines.top':False,'axes.spines.right':False})
fig,axes=plt.subplots(1,2,figsize=(12,5),layout='constrained')
arms=['no-cache','cache'];colors=['#586D79','#177D84']
for ax,key,title,ylabel in [(axes[0],'mean_ms','Mean response time','Milliseconds'),(axes[1],'successful_llm_completions','LLM completions','Successful logical calls')]:
    values=[stats[a][key] for a in arms]
    bars=ax.bar(['Without cache','With cache'],values,color=colors,width=.55)
    ax.bar_label(bars,fmt='%.1f' if key=='mean_ms' else '%.0f',padding=5)
    ax.set(title=title,ylabel=ylabel);ax.set_ylim(0,max(values)*1.2 or 1)
fig.suptitle(f"Measured comparison: {stats['cache']['requests']} requests per arm")
fig.savefig(args.run/'comparison.png',dpi=220);plt.close(fig)
rows=[json.loads(line) for line in (args.run/'requests.jsonl').read_text(encoding='utf8').splitlines()]
fig,ax=plt.subplots(figsize=(12,4),layout='constrained')
for arm,color in zip(arms,colors):
    vals=[r['client_ms'] for r in rows if r['arm']==arm and r['status']=='ok']
    ax.plot(range(1,len(vals)+1),vals,marker='o',label=arm,color=color)
ax.set(title='Response time for every recorded request',xlabel='Request within arm (trials concatenated)',ylabel='Milliseconds')
ax.legend();fig.savefig(args.run/'per_request.png',dpi=220);plt.close(fig)
print('Plotted measured data only. Review answer quality before presentation.')
