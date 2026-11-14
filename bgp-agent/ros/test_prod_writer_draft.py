"""Static contract checks only; these do not execute RouterOS scripts."""
from pathlib import Path
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
        self.assertIn('name=szhome-mosdns-bgp-writer', WRITER)
        for endpoint in ("/guardwriter/next", "/guardwriter/ack"):
            self.assertIn(endpoint, WRITER)
        for action in ("create_guard", "install_barrier", "observe_conntrack",
                       "observe_withdrawn", "remove_guard", "remove_barrier"):
            self.assertIn('"' + action + '"', WRITER)
        for field in ("epoch", "operation_id", "generation", "prefix", "action",
                      "parent_operation_id", "proof", "exact_owner", "guard_present",
                      "barrier_present", "barrier_absent", "bgp_active", "bgp_gone",
                      "no_connections", "rechecked"):
            self.assertIn('"' + field + '"', WRITER)

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


if __name__ == "__main__":
    unittest.main()
