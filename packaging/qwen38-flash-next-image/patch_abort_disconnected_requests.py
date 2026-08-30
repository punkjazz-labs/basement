"""Abort scheduler work before discarding a disconnected request's state.

This applies the narrow source change proposed in sgl-project/sglang#35936 at
commit 764f0b95c64456b67c3aa8a344aeb8308c23c24b. The pinned base predates that
change: its exception cleanup removes ``rid_to_state`` first, after which the
ordinary abort path refuses to tell the scheduler to stop the request.

The exact anchors make a changed base image fail its build instead of silently
shipping without the cancellation guarantee.
"""

from pathlib import Path
import sys


path = Path(sys.argv[1]) if len(sys.argv) == 2 else Path(
    "/sgl-workspace/sglang/python/sglang/srt/managers/tokenizer_manager.py"
)
source = path.read_text()

old_handler = '''        except BaseException:
            # _init_req_state created a rid_to_state entry per (sub-)request up
            # front. The normal remover is the scheduler-response path
            # (_handle_batch_output), so a failure *before* a request reaches the
            # scheduler -- e.g. input-length validation rejecting an over-context
            # request -- would otherwise leak those entries forever. Drop any that
            # are still pending; entries already removed on the normal completion
            # path are left untouched (pop is a no-op).
            self._discard_pending_req_states(obj)
            raise
'''

new_handler = '''        except BaseException:
            # _init_req_state created a rid_to_state entry per (sub-)request up
            # front. The normal remover is the scheduler-response path
            # (_handle_batch_output), so entries still present here belong to
            # requests that never completed -- including CancelledError/
            # GeneratorExit from client disconnects, where the scheduler may
            # still be decoding. Abort those on the scheduler before dropping
            # the HTTP-side state; otherwise abort_request's rid_to_state guard
            # drops the delayed abort and the request runs as a zombie.
            self._abort_and_discard_pending_req_states(obj)
            raise
'''

old_cleanup = '''    def _discard_pending_req_states(self, obj):
        """Drop rid_to_state entries created by _init_req_state for *obj*.

        Safe to call after a partial/failed dispatch: only entries still present
        are removed, and the scheduler-response path looks up state with
        ``.get(...)`` so a later output for a discarded rid is ignored, not fatal.
        """
        if not hasattr(obj, "is_single") or obj.is_single:
            rids = [obj.rid]
        else:
            rids = obj.rid
        for rid in rids:
            self.rid_to_state.pop(rid, None)
'''

new_cleanup = '''    def _abort_and_discard_pending_req_states(self, obj):
        """Abort still-pending requests on the scheduler, then drop their state.

        Dispatch happens before the pop: once rid_to_state is gone, no code path
        is left that can tell the scheduler to stop the request. Only concrete,
        still-tracked request ids are dispatched. A dispatch failure still drops
        local state and does not prevent cleanup of the remaining batch entries.
        """
        if not hasattr(obj, "is_single") or obj.is_single:
            rids = [obj.rid]
        else:
            rids = obj.rid
        for rid in rids:
            if rid in self.rid_to_state:
                try:
                    self._dispatch_to_scheduler(AbortReq(rid=rid))
                except Exception:
                    logger.exception(
                        "Failed to abort request during disconnect cleanup: %s",
                        rid,
                    )
                finally:
                    self.rid_to_state.pop(rid, None)
'''

for name, old in (("exception handler", old_handler), ("cleanup helper", old_cleanup)):
    count = source.count(old)
    if count != 1:
        raise RuntimeError(f"{name} anchor count is {count}, expected exactly 1")

source = source.replace(old_handler, new_handler, 1)
source = source.replace(old_cleanup, new_cleanup, 1)
compile(source, str(path), "exec")
path.write_text(source)
print("tokenizer_manager.py patched to abort disconnected requests")
