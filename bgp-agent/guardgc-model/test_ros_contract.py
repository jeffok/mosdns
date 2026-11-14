"""Reference wire contract tests; this is not the Go/RouterOS transport."""
import ipaddress
import json
import re
import unittest
from pathlib import Path

IDS = re.compile(r"[0-9a-f]{24}\Z")
ROS_ACTIONS = {"install_barrier", "observe_conntrack", "observe_withdrawn", "remove_guard", "remove_barrier"}
OBS_KEYS = {"exact_owner", "barrier_verified", "snapshot_complete", "connections", "guard_present", "bgp_active", "guard_absent", "bgp_absent", "barrier_absent", "rechecked"}


def validate_ack(wire, pending):
    """Reference validator: rejects unknown evidence and maps to guardProof.

    Real adapter and authentication do not exist. This function only specifies
    expected semantics; it must not be used as a production implementation.
    """
    expected_keys = {"version", "epoch", "operation_id", "parent_operation_id", "generation", "prefix", "action", "result", "observation"}
    if set(wire) != expected_keys or type(wire["version"]) is not int or wire["version"] != 1:
        raise ValueError("wire keys/version")
    for key in ("epoch", "operation_id", "generation"):
        if not isinstance(wire[key], str) or not IDS.fullmatch(wire[key]):
            raise ValueError("identity")
    net = ipaddress.ip_network(wire["prefix"], strict=True)
    if net.version != 4 or net.prefixlen != 32 or str(net) != wire["prefix"]:
        raise ValueError("prefix")
    action = wire["action"]
    if action not in ROS_ACTIONS or wire["result"] != "ok":
        raise ValueError("unsupported action/result")
    for key in ("epoch", "generation", "prefix"):
        if wire[key] != pending[key]:
            raise ValueError("identity mismatch")
    if action == "observe_withdrawn":
        if pending["action"] != "withdraw_bgp" or wire["parent_operation_id"] != pending["operation_id"]:
            raise ValueError("parent withdrawal mismatch")
        if wire["operation_id"] == pending["operation_id"]:
            raise ValueError("child probe must have distinct identity")
    elif wire["parent_operation_id"] != "" or wire["operation_id"] != pending["operation_id"] or action != pending["action"]:
        raise ValueError("operation mismatch")
    obs = wire["observation"]
    if not isinstance(obs, dict) or set(obs) != OBS_KEYS:
        raise ValueError("observation keys")
    for key in OBS_KEYS - {"connections"}:
        if type(obs[key]) is not bool:
            raise ValueError("observation boolean")
    if type(obs["connections"]) is not int or obs["connections"] < -1:
        raise ValueError("connection count")
    if obs["snapshot_complete"] != (obs["connections"] >= 0):
        raise ValueError("snapshot consistency")
    if not obs["exact_owner"] or not obs["barrier_verified"]:
        raise ValueError("ownership/barrier")
    no_connections = obs["snapshot_complete"] and obs["connections"] == 0
    if action in ("install_barrier", "observe_conntrack"):
        if not obs["guard_present"] or not obs["bgp_active"] or obs["guard_absent"] or obs["bgp_absent"] or obs["barrier_absent"]:
            raise ValueError("live route proof")
        if action == "observe_conntrack" and not obs["snapshot_complete"]:
            raise ValueError("missing conntrack snapshot")
    elif action == "observe_withdrawn":
        if not obs["guard_present"] or not obs["bgp_absent"] or obs["bgp_active"] or obs["guard_absent"] or obs["barrier_absent"] or not obs["snapshot_complete"]:
            raise ValueError("withdrawn observation")
    elif action == "remove_guard":
        if not (obs["guard_absent"] and obs["bgp_absent"] and no_connections) or obs["barrier_absent"]:
            raise ValueError("remove guard proof")
    elif action == "remove_barrier":
        if not (obs["guard_absent"] and obs["bgp_absent"] and obs["barrier_absent"] and obs["rechecked"] and no_connections):
            raise ValueError("remove barrier proof")
    return {"ExactOwner": obs["exact_owner"],
            "BarrierPresent": obs["barrier_verified"] and not obs["barrier_absent"],
            "BarrierAbsent": obs["barrier_absent"], "GuardPresent": obs["guard_present"],
            "GuardAbsent": obs["guard_absent"], "BGPActive": obs["bgp_active"],
            "BGPGone": obs["bgp_absent"], "NoConnections": no_connections,
            "Rechecked": obs["rechecked"]}


def sample(action, **updates):
    obs = {k: False for k in OBS_KEYS}
    obs.update(connections=-1, exact_owner=True, barrier_verified=True)
    if action in ("install_barrier", "observe_conntrack"):
        obs.update(guard_present=True, bgp_active=True)
    if action != "install_barrier":
        obs.update(snapshot_complete=True, connections=0)
    if action == "observe_withdrawn":
        obs.update(guard_present=True, bgp_absent=True)
    if action in ("remove_guard", "remove_barrier"):
        obs.update(guard_absent=True, bgp_absent=True)
    if action == "remove_barrier":
        obs.update(barrier_absent=True, rechecked=True)
    obs.update(updates)
    return {"version": 1, "epoch": "a" * 24, "operation_id": "b" * 24,
            "parent_operation_id": "c" * 24 if action == "observe_withdrawn" else "",
            "generation": "d" * 24, "prefix": "20.205.243.166/32",
            "action": action, "result": "ok", "observation": obs}


def pending_for(ack):
    return {"version": 1, "epoch": ack["epoch"],
            "operation_id": ack["parent_operation_id"] if ack["action"] == "observe_withdrawn" else ack["operation_id"],
            "generation": ack["generation"], "prefix": ack["prefix"],
            "action": "withdraw_bgp" if ack["action"] == "observe_withdrawn" else ack["action"]}


class ContractTests(unittest.TestCase):
    def test_ros_go_agree_on_action_identifiers_and_observation_fields(self):
        root = Path(__file__).resolve().parents[1]
        script = (root / "ros/production-guardgc-draft.rsc").read_text()
        go = (root / "guard_gc.go").read_text()
        for action in ROS_ACTIONS:
            self.assertIn(f'($action != "{action}")', script)
        for action in (ROS_ACTIONS - {"observe_withdrawn"}) | {"create_guard", "withdraw_bgp"}:
            self.assertIn(f'"{action}"', go)
        for key in OBS_KEYS:
            self.assertIn(f'"{key}"=', script)
        for key in ("version", "epoch", "operation_id", "generation", "prefix", "action"):
            self.assertIn(f'json:"{key}"', go)
            self.assertIn(f'"{key}"=', script)
        self.assertIn(':local enabled false', script)
        self.assertIn(':local mutationEnabled false', script)
        self.assertNotIn('/system scheduler add', script)

    def test_each_action_produces_expected_proof(self):
        for action in ROS_ACTIONS:
            with self.subTest(action=action):
                ack = json.loads(json.dumps(sample(action)))
                proof = validate_ack(ack, pending_for(ack))
                self.assertTrue(proof["ExactOwner"])
                if action == "observe_withdrawn":
                    self.assertTrue(proof["BGPGone"] and proof["NoConnections"] and proof["GuardPresent"])
                if action == "remove_barrier":
                    self.assertTrue(proof["BarrierAbsent"] and proof["Rechecked"] and proof["NoConnections"])

    def test_nonzero_conntrack_is_observation_not_permission(self):
        ack = sample("observe_conntrack", connections=4)
        self.assertFalse(validate_ack(ack, pending_for(ack))["NoConnections"])
        ack = sample("remove_guard", connections=4)
        with self.assertRaises(ValueError):
            validate_ack(ack, pending_for(ack))

    def test_stale_parent_wrong_generation_and_absent_fields_fail(self):
        ack = sample("observe_withdrawn")
        pending = pending_for(ack)
        ack["parent_operation_id"] = "e" * 24
        with self.assertRaises(ValueError): validate_ack(ack, pending)
        ack = sample("remove_guard")
        pending = pending_for(ack)
        ack["generation"] = "e" * 24
        with self.assertRaises(ValueError): validate_ack(ack, pending)
        ack = sample("remove_barrier")
        pending = pending_for(ack)
        del ack["observation"]["rechecked"]
        with self.assertRaises(ValueError): validate_ack(ack, pending)

    def test_untrusted_bool_as_integer_and_unknown_fields_fail(self):
        ack = sample("remove_barrier", connections=False)
        with self.assertRaises(ValueError): validate_ack(ack, pending_for(ack))
        ack = sample("install_barrier")
        ack["observation"]["extra"] = True
        with self.assertRaises(ValueError): validate_ack(ack, pending_for(ack))

    def test_never_send_agent_only_actions_to_ros(self):
        for action in ("create_guard", "withdraw_bgp"):
            ack = sample(action)
            with self.assertRaises(ValueError): validate_ack(ack, pending_for(ack))


if __name__ == "__main__":
    unittest.main()
