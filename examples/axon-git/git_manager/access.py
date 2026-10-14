import threading
from queue import Queue
from readerwriterlock import rwlock
from git_manager.manager import GitRepository


class TaskManager:
    def __init__(self, parallel_limit: int):
        """
        Initialize the TaskManager.

        :param parallel_limit: Maximum number of tasks to run in parallel.
        """
        self.parallel_limit = parallel_limit
        self.task_queue = Queue()
        self.locks = {}  # Dictionary to store ReaderWriterLocks for each task type
        self.lock = threading.Lock()  # Lock to protect access to the locks dictionary
        self.threads = []

    def _get_lock(self, task_type: str):
        """
        Get or create a ReaderWriterLock for the given task type.

        :param task_type: The type of task.
        :return: A ReaderWriterLock object.
        """
        with self.lock:
            if task_type not in self.locks:
                self.locks[task_type] = rwlock.RWLockFair()
            return self.locks[task_type]

    def run_task(self, repo: GitRepository, action: callable):
        done_event = threading.Event()
        task = GitTask(repo, self._get_lock(repo.target_dir), action)
        def task_wrapper():
            try:                
                task.run()
                
            finally:
                done_event.set()  # Signal that the task is complete

        self.task_queue.put(task_wrapper)
        done_event.wait()
        return task.result        

    def _worker(self):
        """
        Worker thread to process tasks from the queue.
        """
        while True:
            task = self.task_queue.get()            
            try:
                task()
            except Exception as e:
                print(f"Error executing task: {e}")                
            self.task_queue.task_done()

    def run(self):
        """
        Start processing tasks with the specified parallelization limit.
        """
        for _ in range(self.parallel_limit):
            thread = threading.Thread(target=self._worker)
            thread.start()
            self.threads.append(thread)        

class GitTask:
    def __init__(self, repo: GitRepository, rwlock: rwlock, action: callable):
        self.repo = repo
        self.action = action
        self.rwlock = rwlock
        self.result = None

    def key(self):
        return self.repo.target_dir

    def run(self):
        if self.repo.needs_update():
            wlock = self.rwlock.gen_wlock()
            with wlock:
                try:
                    self.repo.update(force=True)                    
                except Exception as e:
                    print(f"Error executing task {task.key()}: {e}")
                    raise e

        with self.rwlock.gen_rlock():
            try:
                self.result = self.action()
                return self.result
            except Exception as e:
                print(f"Error executing task {task.key()}: {e}")
                raise e

        