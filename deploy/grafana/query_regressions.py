#!/usr/bin/env python3
"""Generate promtool test rules from the shipped dashboard (stdlib only).

  python3 deploy/grafana/query_regressions.py > /tmp/dashboard-tests.yml
  promtool test rules /tmp/dashboard-tests.yml

The test engine evaluates the actual shipped queries, including NaN fallbacks.
No exporter, model inference or network access is needed.
"""
import json
from pathlib import Path

DASHBOARD = json.loads(Path(__file__).with_name('dashboard.json').read_text())
TABLE = next(p for p in DASHBOARD['panels'] if p['type'] == 'table')
IDENTITIES = [dict(model='same-long-model:9b', family='fixture', quant='q4', job='ollama', instance=instance, cluster=cluster)
              for instance, cluster in [('a:9400', 'blue'), ('b:9400', 'blue'), ('a:9400', 'red')]]
IDENTITIES.append(dict(IDENTITIES[0], model='unloaded:4b'))
REMOVED = dict(IDENTITIES[0], model='deleted:4b')


def labels(values):
    return '{' + ','.join(k + '=' + json.dumps(v) for k, v in sorted(values.items())) + '}'


series = []

def add(metric, identity, values):
    series.append({'series': metric + labels(identity), 'values': values})


for i, identity in enumerate(IDENTITIES):
    add('ollama_model_loaded', identity, ('0' if i == 3 else '1') + 'x24')
    if i == 3:
        add('ollama_model_vram_bytes', identity, '0x24')
        continue
    add('ollama_model_vram_bytes', identity, str((i + 1) * 1024) + 'x24')
    add('ollama_tokens_per_second', identity, str(i * 10) + 'x24')
    if i != 1:
        add('ollama_prompt_tokens_per_second', identity, str(i * 100) + 'x24')
    add('ollama_requests_total', dict(identity, endpoint='/api/chat'), '0+10x24')
    add('ollama_requests_total', dict(identity, endpoint='/api/generate'), '0+20x24')
add('ollama_model_loaded', REMOVED, '1x22 stale')
for metric in ['ollama_model_vram_bytes', 'ollama_tokens_per_second', 'ollama_prompt_tokens_per_second']:
    add(metric, REMOVED, '9999x24')
add('ollama_requests_total', dict(REMOVED, endpoint='/api/chat'), '0+10x24')
# A mismatched extra scrape label must not attach to another identity.
add('ollama_prompt_tokens_per_second', dict(IDENTITIES[1], cluster='other'), '9999x24')
add('ollama_up', {'job':'ollama','instance':'a:9400'}, '1x24')
add('ollama_up', {'job':'ollama','instance':'b:9400'}, '0x24')
expected = [[1,1,1,0], [1024,2048,3072,0], [0,10,20,'__NaN__'], [0,'__NaN__',200,'__NaN__'], [720,720,720,'__NaN__']]
tests = []
for target, values in zip(TABLE['targets'], expected):
    # Keep the query-generated column label: tests cover the real table input.
    column = ['Loaded','VRAM','Decode','Prefill','Requests'][len(tests)]
    tests.append({'expr':target['expr'].replace('$endpoint','.+'), 'eval_time':'24h', 'exp_samples':[
        {'labels':labels(dict(identity, __dashboard_field__=column)), 'value':value}
        for identity, value in zip(IDENTITIES,values)]})
request_target = TABLE['targets'][-1]
tests.append({'expr':request_target['expr'].replace('$endpoint','/api/chat'), 'eval_time':'24h', 'exp_samples':[
    {'labels':labels(dict(identity, __dashboard_field__='Requests')), 'value':240 if i < 3 else '__NaN__'}
    for i,identity in enumerate(IDENTITIES)]})
for panel,values in zip(DASHBOARD['panels'][:4],[[1,2],[3],[6144],[2400]]):
    for target,value in zip(panel['targets'],values):
        tests.append({'expr':target['expr'].replace('$endpoint','.+'),'eval_time':'24h','exp_samples':[{'labels':'{}','value':value}]})
# With no inventory all table queries must be empty, even with retained counters.
empty = [{'expr':t['expr'].replace('$endpoint','.+'),'eval_time':'24h','exp_samples':[]} for t in TABLE['targets']]
# Promtool 2.51 compares expected NaN with IEEE equality (NaN != NaN).
# Check known samples separately and assert each unknown via x == bool x.
checked = []
for case in tests:
    if any(s['value'] == '__NaN__' for s in case['exp_samples']):
        expr = case['expr']
        checked.append(dict(case, expr=f'({expr}) == ({expr})',
                            exp_samples=[s for s in case['exp_samples'] if s['value'] != '__NaN__']))
        checked.append(dict(case, expr=f'({expr}) == bool ({expr})',
                            exp_samples=[dict(s, value=0 if s['value'] == '__NaN__' else 1)
                                         for s in case['exp_samples']]))
    else:
        checked.append(case)
tests = checked
result={'rule_files' :[],'evaluation_interval':'1h','tests':[
    {'name':'full identity, zeros, missing values, endpoint aggregation and deleted inventory','interval':'1h','input_series':series,'promql_expr_test':tests},
    {'name':'no inventory does not resurrect retained counters','interval':'1h','input_series':[s for s in series if s['series'].startswith('ollama_requests_total')],'promql_expr_test':empty}]}
# A partial memory total must not silently under-report missing model memory.
vram_total = DASHBOARD['panels'][2]['targets'][0]['expr']
result['tests'].append({'name':'partial memory total is unknown', 'interval':'1h',
    'input_series':[s for s in series if not (s['series'].startswith('ollama_model_vram_bytes') and 'instance="b:9400"' in s['series'])],
    'promql_expr_test':[{'expr':f'({vram_total}) == bool ({vram_total})','eval_time':'24h',
                         'exp_samples':[{'labels':'{}','value':0}]}]})
# Range graphs must emit an explicit NaN at an empty evaluation, preventing
# last-sample reduction from carrying stale totals across missing data.
result['tests'].append({'name':'empty aggregate evaluation stays unknown','interval':'1h','input_series':[],
    'promql_expr_test':[{'expr':f'({q["expr"].replace("$endpoint", ".+")}) == bool ({q["expr"].replace("$endpoint", ".+")})',
                        'eval_time':'24h','exp_samples':[{'labels':'{}','value':0}]}
                       for p in DASHBOARD['panels'][:4] for q in p['targets']]})
print(json.dumps(result,indent=2))
