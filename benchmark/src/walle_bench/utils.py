import ctypes
import threading
from contextlib import contextmanager


@contextmanager
def timeout(seconds):
    """Raise ``TimeoutError`` if the block takes longer than ``seconds``.

    ``threading.Timer`` runs its callback in a different thread, so raising from
    the callback only fails that timer thread. For this benchmark helper we need
    to interrupt the thread that entered the context manager. On CPython,
    ``PyThreadState_SetAsyncExc`` can schedule an exception in that thread.
    """

    if seconds <= 0:
        raise ValueError("seconds must be positive")

    timed_out = threading.Event()
    finished = threading.Event()
    target_thread_id = threading.get_ident()

    def _raise_timeout():
        if finished.is_set():
            return

        timed_out.set()
        result = ctypes.pythonapi.PyThreadState_SetAsyncExc(
            ctypes.c_ulong(target_thread_id),
            ctypes.py_object(TimeoutError),
        )
        if result > 1:
            # Undo if CPython reports that more than one thread was affected.
            ctypes.pythonapi.PyThreadState_SetAsyncExc(
                ctypes.c_ulong(target_thread_id),
                None,
            )

    timer = threading.Timer(seconds, _raise_timeout)
    timer.daemon = True
    timer.start()

    try:
        yield
    finally:
        finished.set()
        timer.cancel()
