"""Executable concurrency/crash model, NOT production RouterOS code."""
import copy
import threading
import unittest
from dataclasses import dataclass


@dataclass
class Op:
    seq: int
    generation: int
    kind: str


class RouterModel:
    """External effects stand for verified and persistent ROS/GoBGP state."""
    def __init__(self):
        self.route = True
        self.guard = True
        self.barrier = False
        self.connections = 0
        self.applied = set()

    def effect(self, op):
        key = (op.seq, op.generation, op.kind)
        if key in self.applied:
            return
        if op.kind == "barrier_add":
            self.barrier = True
        elif op.kind == "withdraw":
            assert self.barrier and self.guard and self.connections == 0
            self.route = False
        elif op.kind == "guard_remove":
            assert self.barrier and not self.route and self.connections == 0
            self.guard = False
        elif op.kind == "barrier_remove":
            assert self.barrier and not self.guard and not self.route and self.connections == 0
            self.barrier = False
        else:
            raise ValueError(op.kind)
        self.applied.add(key)


class LifecycleModel:
    """One serialized prefix, with a durable journal and explicit ROS effects.

    save() represents successful durable fsync, not actual filesystem I/O.
    One RouterOS writer and full conntrack coverage are assumptions.
    """
    def __init__(self, router=None):
        self.lock = threading.RLock()
        self.router = router or RouterModel()
        self.record = dict(generation=1, phase="ACTIVE", horizon=0,
                           pins={}, pin_seq=0, seq=0, pending=None, barrier_at=None)
        self.disk = None
        self.save()

    def save(self):
        self.disk = copy.deepcopy(self.record)

    def crash_recover(self, now, max_ttl=300, margin=5):
        with self.lock:
            self.record = copy.deepcopy(self.disk)
            self.record["pins"] = {}  # prior process can no longer emit
            if self.record["phase"] in ("ACTIVE", "DRAINING"):
                self.record["horizon"] = max(self.record["horizon"], now + max_ttl + margin)
            if self.record["phase"] == "BARRIER":
                self.record["barrier_at"] = now  # new monotonic quiet window
            self.save()

    def prepare_answer(self, now, ttl, max_send_delay=2):
        with self.lock:
            s = self.record
            if s["phase"] != "ACTIVE" or s["pending"] is not None:
                raise RuntimeError("no new DNS answers during retirement")
            if not self.router.route or not self.router.guard or self.router.barrier:
                raise RuntimeError("BGP/guard not confirmed or barrier active")
            if not 0 <= ttl <= 300:
                raise ValueError("invalid TTL")
            s["horizon"] = max(s["horizon"], now + ttl + max_send_delay)
            s["pin_seq"] += 1
            pin = s["pin_seq"]
            s["pins"][pin] = now + max_send_delay
            self.save()  # commit response horizon before a positive response
            return (s["generation"], pin, now + max_send_delay)

    def finish_answer(self, ticket, now):
        with self.lock:
            generation, pin, deadline = ticket
            s = self.record
            if s["pins"].get(pin) != deadline or generation != s["generation"]:
                raise RuntimeError("stale answer pin")
            del s["pins"][pin]
            self.save()
            return now <= deadline  # caller must not emit if False

    def begin_retire(self):
        with self.lock:
            s = self.record
            if s["phase"] != "ACTIVE" or s["pending"] or s["pins"]:
                raise RuntimeError("outstanding positive answer or operation")
            s["phase"] = "DRAINING"
            self.save()

    def start_op(self, kind, now, safety=5, quiet=5):
        with self.lock:
            s, r = self.record, self.router
            if s["pending"] is not None:
                raise RuntimeError("one prefix operation at a time")
            if kind == "barrier_add":
                valid = s["phase"] == "DRAINING" and not s["pins"] and now >= s["horizon"] + safety
            elif kind == "withdraw":
                valid = s["phase"] == "BARRIER" and r.barrier and r.guard and r.connections == 0 and now >= s["barrier_at"] + quiet and now >= s["horizon"] + safety
            elif kind == "guard_remove":
                valid = s["phase"] == "WITHDRAWN" and r.barrier and not r.route and r.connections == 0 and now >= s["horizon"] + safety
            elif kind == "barrier_remove":
                valid = s["phase"] == "GUARD_REMOVED" and r.barrier and not r.route and not r.guard and r.connections == 0 and now >= s["horizon"] + safety
            else:
                raise ValueError(kind)
            if not valid:
                raise RuntimeError("unsafe transition")
            s["seq"] += 1
            s["pending"] = Op(s["seq"], s["generation"], kind)
            self.save()  # durable intent before the external effect
            return copy.deepcopy(s["pending"])

    def execute(self, op):
        with self.lock:
            if self.record["pending"] != op or op.generation != self.record["generation"]:
                raise RuntimeError("stale or non-current operation")
            self.router.effect(op)  # ROS readback/idempotent effect assumed

    def ack(self, op, now):
        with self.lock:
            s, r = self.record, self.router
            if s["pending"] != op or op.generation != s["generation"]:
                raise RuntimeError("replayed or superseded ACK")
            proof = {"barrier_add": r.barrier,
                     "withdraw": r.barrier and not r.route and r.connections == 0,
                     "guard_remove": r.barrier and not r.route and not r.guard and r.connections == 0,
                     "barrier_remove": not r.barrier and not r.route and not r.guard and r.connections == 0}
            if not proof[op.kind]:
                raise RuntimeError("missing effect readback")
            s["phase"] = {"barrier_add": "BARRIER", "withdraw": "WITHDRAWN",
                          "guard_remove": "GUARD_REMOVED", "barrier_remove": "RETIRED"}[op.kind]
            if op.kind == "barrier_add":
                s["barrier_at"] = now
            s["pending"] = None
            self.save()

    def new_generation(self):
        with self.lock:
            s = self.record
            if s["phase"] != "RETIRED" or s["pending"]:
                raise RuntimeError("cleanup unfinished")
            # This abstract reset assumes new guard + route readbacks, in order,
            # before answer admission. Production must model both as operations.
            self.router.guard = True
            self.router.route = True
            s.update(generation=s["generation"] + 1, phase="ACTIVE", horizon=0,
                     pins={}, barrier_at=None)
            self.save()


class LifecycleTests(unittest.TestCase):
    def apply(self, m, kind, now):
        op = m.start_op(kind, now)
        m.execute(op)
        m.ack(op, now)
        return op

    def test_ttl_send_pin_and_barrier_order(self):
        m = LifecycleModel()
        ticket = m.prepare_answer(10, 300, 2)
        with self.assertRaises(RuntimeError):
            m.begin_retire()  # in-flight DNS write
        self.assertTrue(m.finish_answer(ticket, 11))
        m.begin_retire()
        with self.assertRaises(RuntimeError):
            m.prepare_answer(12, 100)
        with self.assertRaises(RuntimeError):
            m.start_op("barrier_add", 316)  # 10 + 300 + 2 + safety 5 = 317
        self.apply(m, "barrier_add", 317)
        m.router.connections = 1
        with self.assertRaises(RuntimeError):
            m.start_op("withdraw", 323)
        m.router.connections = 0
        self.apply(m, "withdraw", 323)
        self.apply(m, "guard_remove", 324)
        self.assertTrue(m.router.barrier)  # no direct fallback yet
        self.apply(m, "barrier_remove", 325)
        self.assertEqual(m.record["phase"], "RETIRED")

    def test_duplicate_pin_cannot_release_another_answer(self):
        m = LifecycleModel()
        first = m.prepare_answer(10, 30)
        second = m.prepare_answer(10, 30)
        self.assertTrue(m.finish_answer(first, 11))
        with self.assertRaises(RuntimeError):
            m.finish_answer(first, 11)
        with self.assertRaises(RuntimeError):
            m.begin_retire()
        self.assertTrue(m.finish_answer(second, 11))
        m.begin_retire()

    def test_expired_dns_send_is_not_emitted(self):
        m = LifecycleModel()
        ticket = m.prepare_answer(0, 300, 2)
        self.assertFalse(m.finish_answer(ticket, 3))
        m.begin_retire()
        with self.assertRaises(RuntimeError):
            m.start_op("barrier_add", 306)

    def test_crash_after_each_effect_reconciles_same_operation(self):
        m = LifecycleModel()
        m.begin_retire()
        for kind, now in (("barrier_add", 10), ("withdraw", 320),
                          ("guard_remove", 321), ("barrier_remove", 322)):
            op = m.start_op(kind, now)
            m.execute(op)
            m.crash_recover(now)  # durable intent survives; no new generation
            with self.assertRaises(RuntimeError):
                m.new_generation()
            self.assertEqual(m.record["pending"], op)
            m.execute(op)  # idempotent retry/readback
            m.ack(op, now)
        self.assertEqual(m.record["phase"], "RETIRED")

    def test_old_ack_and_operation_cannot_touch_new_generation(self):
        m = LifecycleModel()
        m.begin_retire()
        first = self.apply(m, "barrier_add", 10)
        self.apply(m, "withdraw", 16)
        self.apply(m, "guard_remove", 17)
        self.apply(m, "barrier_remove", 18)
        m.new_generation()
        with self.assertRaises(RuntimeError):
            m.execute(first)
        with self.assertRaises(RuntimeError):
            m.ack(first, 19)
        self.assertTrue(m.router.guard and m.router.route and not m.router.barrier)

    def test_concurrent_retire_rejects_readd_and_old_token(self):
        m = LifecycleModel()
        m.begin_retire()
        op = m.start_op("barrier_add", 10)
        failures = []
        def race():
            for fn in (lambda: m.prepare_answer(11, 300), m.new_generation):
                try:
                    fn()
                    failures.append("unexpected acceptance")
                except RuntimeError:
                    pass
        workers = [threading.Thread(target=race) for _ in range(8)]
        for t in workers: t.start()
        for t in workers: t.join()
        self.assertEqual(failures, [])
        m.execute(op)
        m.ack(op, 11)
        with self.assertRaises(RuntimeError):
            m.ack(op, 12)

    def test_restart_extends_cache_grace_and_guard_failure_closes(self):
        m = LifecycleModel()
        ticket = m.prepare_answer(0, 1)
        self.assertTrue(m.finish_answer(ticket, 1))
        m.crash_recover(10)
        m.begin_retire()
        with self.assertRaises(RuntimeError):
            m.start_op("barrier_add", 314)  # restart 10 + 300 + 5 + safety 5
        self.apply(m, "barrier_add", 320)
        n = LifecycleModel()
        n.router.guard = False
        with self.assertRaises(RuntimeError):
            n.prepare_answer(1, 20)


if __name__ == "__main__":
    unittest.main()
