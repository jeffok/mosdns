"""Static contract checks only; these do not execute RouterOS scripts."""
from pathlib import Path
import re
import subprocess
import unittest

ROOT = Path(__file__).resolve().parent
WRITER = (ROOT / "prod-writer-draft.rsc").read_text()
PRESTAGE = (ROOT / "prod-prestage.rsc").read_text()


class DisabledWriterDraftTests(unittest.TestCase):
    def test_no_scheduler_and_mutations_disabled(self):
        self.assertIn(":local enabled false", WRITER)
        self.assertIn(":local mutationEnabled false", WRITER)
        self.assertNotIn("/system scheduler add", WRITER + PRESTAGE)
        self.assertIn("disabled=yes", PRESTAGE)

    def test_single_writer_and_protocol(self):
        self.assertIn('name=mosdns-writer', WRITER)
        for endpoint in ("/guardwriter/next", "/guardwriter/ack"):
            self.assertIn(endpoint, WRITER)
        for action in ("create_guard", "confirm_bgp", "remove_orphan_guard", "install_barrier", "observe_conntrack",
                       "observe_withdrawn", "remove_guard", "remove_barrier"):
            self.assertIn('"' + action + '"', WRITER)
        for field in ("epoch", "operation_id", "generation", "prefix", "action",
                      "parent_operation_id", "proof", "exact_owner", "guard_present", "guard_absent",
                      "barrier_present", "barrier_absent", "bgp_active", "bgp_gone",
                      "no_connections", "rechecked"):
            self.assertIn('"' + field + '"', WRITER)

    def test_guard_removal_proof_is_sent_in_payload(self):
        proof = WRITER.split(':local proof {"exact_owner"=($obs->"exact_owner")', 1)[1].split('\n', 1)[0]
        self.assertIn('"guard_absent"=($obs->"guard_absent")', proof)
        self.assertIn('"proof"=$proof', WRITER)

    def test_guard_created_before_bgp_confirmation(self):
        branch = WRITER.split(':if (($action = "create_guard") || ($action = "confirm_bgp") || ($action = "remove_orphan_guard")) do={', 1)[1]
        before_confirm, confirm = branch.split(':if ($action = "confirm_bgp") do={', 1)
        self.assertIn('/ip route add dst-address=$prefix', before_confirm)
        self.assertIn('BGP existed before guard', before_confirm)
        self.assertNotIn('active BGP proof unavailable', before_confirm)
        self.assertIn('active BGP proof unavailable', confirm)

    def test_orphan_guard_checks_before_and_after_deletion(self):
        orphan = WRITER.split(':if ($action = "remove_orphan_guard") do={', 1)[1].split('} else={', 1)[0]
        for text in ('orphan has BGP route', 'orphan has barrier', 'orphan guard ownership conflict',
                     'invalid orphan guard', 'orphan removal readback failed'):
            self.assertIn(text, orphan)
        self.assertIn('"guard_absent") true', orphan)
        self.assertIn('"bgp_absent") true', orphan)

    def test_guard_removal_replay_keeps_preconditions(self):
        start = WRITER.index(':if (($action = "remove_guard") || ($action = "remove_barrier")) do={')
        end = WRITER.index(':if ($action = "remove_barrier") do={', start)
        block = WRITER[start:end]
        self.assertIn('BGP route still present', block)
        self.assertIn('duplicate or foreign-generation guard', block)
        self.assertIn(':if ([:len $owned] = 1) do={', block)
        self.assertIn('guard still present', block)

    def test_batch_stops_after_failure_and_preserves_ids(self):
        batch = WRITER.split('url="http://192.168.88.222:9080/guardwriter/batch-next"', 1)[1].split(':local writerContinue true', 1)[0]
        self.assertIn('[:len $ops] > 8', batch)
        self.assertIn('($batch->"batch_id")', batch)
        self.assertIn('($batch->"operations")', batch)
        self.assertIn(':local result "blocked"', batch)
        self.assertIn('on-error={ :set stopped true }', batch)
        self.assertIn(':set result "ok"', batch)
        self.assertIn('"batch_id"=$batchID;"results"=$results', batch)
        self.assertIn('"operation"=$op;"result"=$result;"proof"=$proof', batch)
        self.assertIn(':set ($results->[:len $results]) {', batch)
        self.assertIn('/guardwriter/batch-ack', batch)
        self.assertIn('[:len $payload] > 3900', batch)
        self.assertIn('($didBatch = false)', WRITER)

    def test_production_names_match(self):
        self.assertIn('/routing bgp instance add name=mosdns ', PRESTAGE)
        self.assertIn('/routing bgp connection add name=mosdns instance=mosdns ', PRESTAGE)
        self.assertIn('input.filter=mosdns-in', PRESTAGE)
        self.assertIn('output.filter-chain=mosdns-out', PRESTAGE)
        self.assertIn('/system script add name=mosdns-writer ', WRITER)
        self.assertIn('routing-table=main', PRESTAGE)

    def test_health_guard_label_cannot_mask_crossed_destinations(self):
        def canonical(prefix):
            return prefix if '/' in prefix else prefix + '/32'
        def accepted(prefix, generation, routes):
            counts, labels, label_counts = {}, {}, {}
            for label, dst in routes:
                dst = canonical(dst)
                counts[dst] = counts.get(dst, 0) + 1
                labels[label] = dst
                label_counts[label] = label_counts.get(label, 0) + 1
            label = f'BGP-PROD-BH:{generation}:{prefix}'
            return counts.get(prefix, 0) == 1 and label_counts.get(label, 0) == 1 and labels.get(label) == prefix
        p, q, gen = '8.8.4.4/32', '9.9.9.9/32', 'a' * 24
        self.assertTrue(accepted(p, gen, [(f'BGP-PROD-BH:{gen}:{p}', '8.8.4.4')]))
        self.assertFalse(accepted(p, gen, [(f'BGP-PROD-BH:{gen}:{p}', q), ('foreign', p)]))
        self.assertFalse(accepted(p, gen, [(f'BGP-PROD-BH:{gen}:{p}', q), (f'BGP-PROD-BH:{gen}:{p}', p)]))

    def test_bulk_health_uses_one_snapshot_per_class(self):
        health = WRITER.split('url="http://192.168.88.222:9080/guardwriter/health?bulk=1"', 1)[1]
        self.assertEqual(health.count('/ip route print detail as-value'), 2)
        self.assertNotIn('/ip route find', health)
        self.assertNotIn('/ip route get', health)
        for field in ('"generation"', '"epoch"', '"snapshot_id"', '"accepted_indexes"'):
            self.assertIn(field, health)
        self.assertIn('/guardwriter/health-ack', health)
        self.assertIn('($healthPages != 1)', health)
        self.assertIn(':set accepted ($accepted, $entryIndex)', health)
        self.assertIn(':set entryIndex ($entryIndex + 1)', health)
        # Labels must bind to the destination actually printed by ROS. A
        # correct label attached to a different prefix must never be accepted.
        self.assertIn(':set ($guardOK->$label) $prefix', health)
        self.assertIn('(($guardOK->$label) = $prefix)', health)
        self.assertIn('(($guardLabels->$label) = 1)', health)
        self.assertIn('[:typeof [:find $prefix "/"]] = "nil"', health)

    def test_recovery_is_paged_and_checks_unexpected_objects(self):
        recovery = WRITER.split('/guardrecovery/next', 1)[1].split('} on-error={', 1)[0]
        for item in ('/guardrecovery/attest', 'unexpected_objects', 'writer_fenced',
                     'old_jobs_quiescent', 'objects_complete', 'total_pages',
                     'pending_id', 'guard_owned', 'bgp_verified', 'barrier_owned'):
            self.assertIn(item, recovery)
        self.assertIn('(($count + 7) / 8)', recovery)
        self.assertIn('old writer job still running', recovery)
        self.assertIn('old writer scheduler enabled', recovery)
        self.assertIn('($seenBarriers->$label) != true', recovery)
        self.assertIn(':set ($guardGood->$label) $prefix', recovery)
        self.assertIn('(($guardGood->$guardLabel) = $prefix)', recovery)
        self.assertIn('(($guardLabelCounts->$guardLabel) = 1)', recovery)
        self.assertIn('recovery guard label already bound elsewhere', recovery)
        self.assertIn(':set ($observations->[:len $observations]) {', recovery)

    def test_no_object_map_concatenated_to_array(self):
        for name in ('prod-writer-draft.rsc', 'iso-writer.rsc',
                     'production-guardgc-draft.rsc', 'ack-blackhole-test.rsc', 'ack-script.rsc'):
            self.assertIsNone(re.search(r':set\s+\w+\s+\(\$\w+,\s*\{"', (ROOT / name).read_text()), name)

    def test_writer_batch_is_bounded_and_yields_on_idle_or_error(self):
        writer = WRITER.split(':local writerContinue true', 1)[1].split('# Keep route health', 1)[0]
        self.assertIn('$writerCount < 32', writer)
        self.assertIn('< 750ms', writer)
        self.assertIn(':if ($action = "idle") do={ :set writerContinue false }', writer)
        self.assertIn('on-error={', writer)
        self.assertIn(':set writerContinue false', writer.split('on-error={', 1)[1])

    def test_create_guard_before_ack_and_reject_bootstrap(self):
        create = WRITER.index('/ip route add dst-address=$prefix')
        verify = WRITER.index(':local good [/ip route find', create)
        ack = WRITER.index('url="http://192.168.88.222:9080/guardwriter/ack"')
        self.assertLess(create, verify)
        self.assertLess(verify, ack)
        for host in ("183.56.180.152", "8.8.8.8", "1.1.1.1"):
            self.assertIn('$host = "' + host + '"', WRITER)
            self.assertIn('dst in ' + host + '/32', PRESTAGE)

    def test_no_dns_change(self):
        self.assertNotIn('/ip dns set', WRITER + PRESTAGE)

    def test_isolated_writer_is_generated_and_source_restricted(self):
        subprocess.run(['python3', str(ROOT / 'make_isolated_writer.py')], check=True)
        writer = (ROOT / 'iso-writer.rsc').read_text()
        setup = (ROOT / 'iso-setup.rsc').read_text()
        teardown = (ROOT / 'iso-teardown.rsc').read_text()
        self.assertNotIn('routing-table="main"', writer + setup)
        self.assertNotIn('routing-table=main ', writer + setup)
        self.assertNotIn('BGP-PROD-', writer)
        self.assertEqual(writer.count('action=drop src-address=192.168.88.222/32'), 2)
        self.assertIn('disabled=yes', setup)
        self.assertNotIn('/ip dns set', writer + setup + teardown)
        self.assertIn('/ip firewall connection remove [find where connection-mark="cm-bgp-iso"]', teardown)
        self.assertNotIn('/ip firewall connection remove [find]', teardown)
        for chain in ('szhome-bgp-iso-in', 'szhome-bgp-iso-out'):
            self.assertIn('chain="' + chain + '"', teardown)

    def test_find_nil_is_compared_by_type(self):
        for name in ('prod-writer-draft.rsc', 'iso-writer.rsc', 'iso-setup.rsc',
                     'production-guardgc-draft.rsc', 'ack-blackhole-test.rsc', 'ack-script.rsc'):
            source = (ROOT / name).read_text()
            self.assertIsNone(re.search(r'(?:=|!=)\s*nil\b', source), name)
        setup = (ROOT / 'iso-setup.rsc').read_text()
        self.assertIn('[:typeof [:find [:tostr [/ip dns get servers]] "192.168.88.222"]] != "nil"', setup)

    def test_json_deserialize_preserves_string_ids(self):
        for name in ('prod-writer-draft.rsc', 'iso-writer.rsc',
                     'production-guardgc-draft.rsc', 'ack-blackhole-test.rsc', 'ack-script.rsc'):
            source = (ROOT / name).read_text()
            self.assertNotIn(':deserialize from=json value=', source, name)
            self.assertIn(':deserialize from=json options=json.no-string-conversion value=', source, name)


if __name__ == "__main__":
    unittest.main()
