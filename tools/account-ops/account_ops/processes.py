"""Only a kernel-identified child group belonging to this job may be signalled."""
import json
import os
from pathlib import Path
import signal
import time
from .security import OpsError,private_file


def boot():return Path('/proc/sys/kernel/random/boot_id').read_text().strip()
def identity(pid):
    try:
        values=Path(f'/proc/{int(pid)}/stat').read_text().rsplit(') ',1)[1].split()
        if values[0]=='Z':return None
        return {'pid':int(pid),'pgid':int(values[2]),'start':values[19],'boot':boot()}
    except (FileNotFoundError,ProcessLookupError):return None


def remember(path,pid):
    record=identity(pid)
    if not record or record['pid']!=record['pgid']:raise OpsError('reconciliation_required',409)
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'w') as stream:json.dump(record,stream);stream.flush();os.fsync(stream.fileno())


def read_owner(path):
    try:record=json.loads(private_file(path))
    except (OSError,ValueError):raise OpsError('reconciliation_required',409) from None
    if set(record)!={'pid','pgid','start','boot'} or type(record['pid']) is not int or record['pid']<=1 or record['pgid']!=record['pid']:raise OpsError('reconciliation_required',409)
    return record


def is_alive(record):
    if record['boot']!=boot():return False
    current=identity(record['pid'])
    if current is not None:return current==record
    # A dead group leader does not prove Chromium descendants exited. Unknown
    # surviving members keep the slot blocked rather than authorizing a signal.
    for entry in Path('/proc').iterdir():
        if not entry.name.isdigit():continue
        try:member=identity(int(entry.name))
        except (PermissionError,ValueError):raise OpsError('reconciliation_required',409) from None
        if member and member['pgid']==record['pgid']:raise OpsError('reconciliation_required',409)
    return False


def terminate(record):
    if not is_alive(record):return
    # Recheck immediately before each signal; PID reuse is never ownership evidence.
    for sig in (signal.SIGTERM,signal.SIGKILL):
        if not is_alive(record):return
        os.killpg(record['pgid'],sig)
        for _ in range(50):
            if not is_alive(record):return
            time.sleep(.1)
    raise OpsError('reconciliation_required',409)
