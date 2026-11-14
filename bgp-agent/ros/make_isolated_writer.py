#!/usr/bin/env python3
"""Generate a disabled, source-restricted writer for rt-bgp-test only."""
from pathlib import Path

base = Path(__file__).resolve().parent
src = (base / "prod-writer-draft.rsc").read_text()
replacements = {
    'mosdns-writer': 'szhome-mosdns-bgp-iso-writer',
    'BGP-PROD-': 'BGP-ISO-',
    'routing-table="main"': 'routing-table="rt-bgp-test"',
    'routing-table=main ': 'routing-table=rt-bgp-test ',
    '= "main")': '= "rt-bgp-test")',
    'chain=$chain action=drop dst-address=$p': 'chain=$chain action=drop src-address=192.168.88.222/32 dst-address=$p',
    '([/ip firewall filter get $rule disabled] != false)': '([/ip firewall filter get $rule disabled] != false) || (([:tostr [/ip firewall filter get $rule src-address]] != "192.168.88.222/32") && ([:tostr [/ip firewall filter get $rule src-address]] != "192.168.88.222"))',
    '(($barrier->"disabled") != true)': '(($barrier->"disabled") != true) && (([:tostr ($barrier->"src-address")] = "192.168.88.222/32") || ([:tostr ($barrier->"src-address")] = "192.168.88.222"))',
}
for before, after in replacements.items():
    if before not in src:
        raise SystemExit(f"required writer pattern missing: {before}")
    src = src.replace(before, after)
# Diagnostics are confined to the isolated copy, sampled every fifth scheduler
# run and whenever a completed operation takes over 300 ms. Keep the same
# single writer and fail-closed control flow as production.
start = src.index('        :local didBatch false')
end = src.index('        :local writerContinue true', start)
batch = src[start:end]
batch = batch.replace('        :do {\n            :local fetched [/tool fetch url="http://192.168.88.222:9080/guardwriter/batch-next"',
                      '        :onerror batchErr in={\n            :local batchStarted [:timestamp]\n            :local fetched [/tool fetch url="http://192.168.88.222:9080/guardwriter/batch-next"', 1)
batch = batch.replace('            :local body [:tostr ($fetched->"data")]',
                      '            :local batchNextElapsed ([:timestamp] - $batchStarted)\n            :local body [:tostr ($fetched->"data")]', 1)
batch = batch.replace('                        :do {\n                            :local action ($op->"action")',
                      '                        :onerror itemErr in={\n                            :local action ($op->"action")', 1)
batch = batch.replace('                        } on-error={ :set stopped true }',
                      '''                        } do={
                            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
                            :if (($runs % 5) = 0) do={ :log warning ("BGP-ISO-PROFILE batch-item-error action=" . ($op->"action") . " prefix=" . ($op->"prefix") . " error=" . $itemErr) }
                            :set stopped true
                        }''', 1)
batch = batch.replace('                :local posted [/tool fetch url="http://192.168.88.222:9080/guardwriter/batch-ack"',
                      '                :local batchAckStarted [:timestamp]\n                :local posted [/tool fetch url="http://192.168.88.222:9080/guardwriter/batch-ack"', 1)
batch = batch.replace('                :if (($posted->"status") != "finished") do={ :error "batch acknowledgement incomplete" }',
                      '''                :if (($posted->"status") != "finished") do={ :error "batch acknowledgement incomplete" }
                :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
                :if ((([:timestamp] - $batchStarted) > 300ms) || (($runs % 5) = 0)) do={ :log warning ("BGP-ISO-PROFILE batch count=" . [:len $ops] . " next=" . $batchNextElapsed . " pre-ack=" . ($batchAckStarted - $batchStarted) . " ack=" . ([:timestamp] - $batchAckStarted) . " total=" . ([:timestamp] - $batchStarted)) }''', 1)
batch = batch.replace('        } on-error={\n            # The agent retains the same batch',
                      '''        } do={
            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
            :if (($runs % 5) = 0) do={ :log warning ("BGP-ISO-PROFILE batch-error=" . $batchErr) }
            # The agent retains the same batch''', 1)
src = src[:start] + batch + src[end:]
start = src.index('        :local writerContinue true')
end = src.index('        # Keep route health', start)
writer = src[start:end]
writer = writer.replace('        :do {\n            :local fetched [/tool fetch url="http://192.168.88.222:9080/guardwriter/next"',
                        '        :onerror writerErr in={\n            :local opStarted [:timestamp]\n            :local fetched [/tool fetch url="http://192.168.88.222:9080/guardwriter/next"', 1)
writer = writer.replace('            :local body [:tostr ($fetched->"data")]',
                        '            :local nextElapsed ([:timestamp] - $opStarted)\n            :local body [:tostr ($fetched->"data")]', 1)
writer = writer.replace('                :local posted [/tool fetch url="http://192.168.88.222:9080/guardwriter/ack"',
                        '                :local ackStarted [:timestamp]\n                :local posted [/tool fetch url="http://192.168.88.222:9080/guardwriter/ack"', 1)
writer = writer.replace('                :if (($posted->"status") != "finished") do={ :error "guard GC acknowledgement incomplete" }',
                        '''                :if (($posted->"status") != "finished") do={ :error "guard GC acknowledgement incomplete" }
                :local elapsed ([:timestamp] - $opStarted)
                :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
                :if (($elapsed > 300ms) || (($runs % 5) = 0)) do={ :log warning ("BGP-ISO-PROFILE op=" . $action . " next=" . $nextElapsed . " pre-ack=" . ($ackStarted - $opStarted) . " ack=" . ([:timestamp] - $ackStarted) . " total=" . $elapsed) }''', 1)
writer = writer.replace('        } on-error={\n            # Unknown or stale evidence',
                        '''        } do={
            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
            :if (($runs % 5) = 0) do={ :log warning ("BGP-ISO-PROFILE writer-error=" . $writerErr) }
            # Unknown or stale evidence''', 1)
src = src[:start] + writer + src[end:]
start = src.index('        # Keep route health')
health = src[start:]
health = health.replace('        :do {\n            :local started [:timestamp]',
                        '        :onerror healthErr in={\n            :local started [:timestamp]', 1)
health = health.replace('        } on-error={\n            # Missing or delayed health',
                        '''            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
            :if ((([:timestamp] - $started) > 300ms) || (($runs % 5) = 0)) do={ :log warning ("BGP-ISO-PROFILE health total=" . ([:timestamp] - $started) . " accepted=" . [:len $accepted]) }
        } do={
            :local runs [/system scheduler get [find where name="szhome-mosdns-bgp-iso-writer"] run-count]
            :if (($runs % 5) = 0) do={ :log warning ("BGP-ISO-PROFILE health-error=" . $healthErr) }
            # Missing or delayed health''', 1)
src = src[:start] + health
for marker in ('BGP-ISO-PROFILE op=', 'BGP-ISO-PROFILE writer-error=',
               'BGP-ISO-PROFILE health total=', 'BGP-ISO-PROFILE health-error=',
               'BGP-ISO-PROFILE batch count=', 'BGP-ISO-PROFILE batch-error=',
               'BGP-ISO-PROFILE batch-item-error action='):
    if marker not in src:
        raise SystemExit(f"diagnostic insertion failed: {marker}")
if 'routing-table="main"' in src or 'routing-table=main ' in src or 'BGP-PROD-' in src:
    raise SystemExit("unisolated production writer reference remains")
if src.count('src-address=192.168.88.222/32 dst-address=$p') != 2:
    raise SystemExit("unexpected drop-rule source restrictions")
if ':local enabled false' not in src or ':local mutationEnabled false' not in src:
    raise SystemExit("writer must be generated disabled")
header = '# GENERATED ISOLATED TEST ONLY: no household DNS or main-table mutations.\n'
(base / 'iso-writer.rsc').write_text(header + src)
