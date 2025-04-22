import pygit2
from pathlib import Path
import os
from datetime import datetime
from git_manager.formatter import GitFormatter

CLONE_DEPTH = 100
UPDATE_TTL_SECONDS = 300

class GitRepositoryManager:

    _credentials_by_type = {
        pygit2.enums.CredentialType.USERNAME: None,
        pygit2.enums.CredentialType.SSH_KEY: None,
        pygit2.enums.CredentialType.USERPASS_PLAINTEXT: None,
    }

    def add_credentials(self, creds):
        existing = GitRepositoryManager._credentials_by_type.get(creds.credential_type)
        if existing:
            raise ValueError(f"Credentials for {creds.credential_type} already set.")
        GitRepositoryManager._credentials_by_type[creds.credential_type] = creds

    repositories = {}
    root_dir: str = None

    def __init__(self, root_dir: str, formatter: GitFormatter):
        self.root_dir = root_dir    
        self.formatter = formatter
        os.makedirs(root_dir, exist_ok=True)    

    def get(self, repo_name: str, branch: str = None) -> 'GitRepository':
        repo_url = self.formatter.repo_url(repo_name)
        repo = GitRepository(repo_name, repo_url, self.root_dir, branch)
        if repo.target_dir not in self.repositories:            
            self.repositories[repo.target_dir] = repo
        return self.repositories[repo.target_dir]


class GitRepository:

    last_updated: datetime = None

    def __init__(self, repo_name: str, repo_url: str, root_dir: str, branch: str = None):
        """
        Initialize the GitRepositoryManager.

        :param repo_url: The HTTPS URL of the repository to clone.
        :param username: The username for authentication.
        :param password: The password or personal access token for authentication.
        :param target_dir: The directory where the repository will be cloned.
        """
        self.repo_url = repo_url
        self.repo_name = repo_name
        self.branch = None
        self.root_dir = root_dir
        self._callbacks = MyRemoteCallbacks(GitRepositoryManager._credentials_by_type)
        self.target_dir = self._get_repo_path(repo_url)

    def needs_update(self) -> bool:
        if not self.last_updated:
            return True

        # Check if the last update was more than 1 hour ago
        now = datetime.now()
        delta = now - self.last_updated
        if delta.total_seconds() > UPDATE_TTL_SECONDS:
            return True

        return False

    def exists(self) -> bool:
        """
        Check if the repository exists in the target directory.

        :return: True if the repository exists, False otherwise.
        """
        if Path(self.target_dir).exists():
            return True

        try:
            self.update()
        except Exception as e:
            self.last_updated = datetime.now()
            pass
        return Path(self.target_dir).exists()
  

    def get_default_branch(self):

        try:
            # Open the repository in read-only mode
            repo = pygit2.Repository(path=self.target_dir)
            # Get the default branch
            self.branch = repo.head.shorthand
            return self.branch
        except Exception as e:
            print(f"Failed to get default branch: {e}")
            raise

    def get_branches(self) -> list[dict]:
        try:
            # Open the repository in read-only mode
            repo = pygit2.Repository(path=self.target_dir)
            # Get the list of branches
            branches = []
            for branch in repo.branches.local:
                branches.append({"name": branch, "head": str(repo.branches.local[branch].raw_target)})
            return branches
        except Exception as e:
            print(f"Failed to get branches: {e}")
            raise

    def _get_repo_path(self, repo_url: str):
        if repo_url.startswith("https://"):
            repo_url = repo_url.replace("https://", "")

        # replace : with _
        repo_url = repo_url.replace(":", "/")
        branch = self.branch if self.branch else "__default__"
        return self.root_dir + "/" + repo_url + "/" + branch

    def _get_file(self, file_path: str) -> Path:

        if file_path.startswith(self.target_dir):
            return Path(file_path)

        full_path = self.target_dir + "/" + file_path.lstrip("/")
        return Path(full_path)

    def get_file_contents_string(self, file_path: str) -> str:
        file = self._get_file(file_path)
        if not file.exists():
            raise FileNotFoundError(f"File {file_path} not found in repository {self.repo_name}")
        with open(file, "r") as f:
            return f.read()
        return None
    
    def get_file_contents_binary(self, file_path: str) -> str:
        file = self._get_file(file_path)
        if not file.exists():
            raise FileNotFoundError(f"File {file_path} not found in repository {self.repo_name}")
       
        # Read binary file content then return it as base64 encoded string
        with open(file, "rb") as f:
            content = f.read()
            return content.decode("utf-8")

        return None

    def file_path_exists(self, file_path: str) -> bool:
        return self._get_file(file_path).exists()

    def _to_repo_relative(self, file_path: str) -> str:
        if file_path.startswith(self.target_dir):
            file_path = file_path[len(self.target_dir):].lstrip("/")
        return file_path

    def get_file_sha(self, file_path: str) -> str:
        try:
            repo = pygit2.Repository(path=self.target_dir)
            rel_path = self._to_repo_relative(file_path)
            blob_sha = repo.get(repo.head.target).tree[rel_path].id
            return str(blob_sha)
        except Exception as e:
            print(f"Failed to get file SHA: {e}")
            return None

    def update(self, force: bool = False) -> str:
        if not force and not self.needs_update():            
            return None

        print(f"Updating repository {self.repo_name}")
        try:
            if not Path(self.target_dir).exists():                
                return self.clone()
                
            self.fetch_and_reset()

        except Exception as e:
            print(f"Failed to update repository, cloning: {e}")
            return self.clone()
            
    def commits(self, limit: int = 10) -> list[pygit2.Commit]:
        repo = pygit2.Repository(path=self.target_dir)    
        last = repo[repo.head.target]
        for commit in repo.walk(last.id, pygit2.enums.SortMode.TIME):
            if limit <= 0:
                break

            if not isinstance(commit, pygit2.Commit):
                continue

            limit -= 1
            yield commit

    def sha(self) -> str:
        try:
            # Open the repository
            repo = pygit2.Repository(path=self.target_dir)
            # Get the current commit SHA
            sha = str(repo.head.raw_target)
            return sha
        except Exception as e:
            print(f"Failed to get SHA: {e}")
            return None

    def fetch_and_reset(self) -> str:
        try:
            # Open the repository
            repo = pygit2.Repository(path=self.target_dir)
            # Fetch the latest changes
            remote = repo.remotes["origin"]
            remote.fetch(callbacks=self._callbacks)
            # Hard reset to the latest commit
            repo.reset(repo.head.target, pygit2.GIT_RESET_HARD)
            self.last_updated = datetime.now()
            print(f"Repository updated successfully in {self.target_dir} to {repo.head.target}")
        except Exception as e:
            print(f"Failed to update repository: {e}")
            raise
            
    def clone(self) -> str:
        try:
            # Clone the repository
            repo = pygit2.clone_repository(
                self.repo_url, 
                self.target_dir, 
                depth=CLONE_DEPTH,
                callbacks=self._callbacks, 
                checkout_branch=self.branch
            )
            self.last_updated = datetime.now()
            print(f"Repository cloned successfully to {self.target_dir} with SHA {repo.head.target}")
            return repo.head.target
        except Exception as e:
            print(f"Failed to clone repository: {e}")
            raise


class MyRemoteCallbacks(pygit2.RemoteCallbacks):

    def __init__(self, credentials_by_type):
        super().__init__()
        self.credentials_by_type = credentials_by_type

    def transfer_progress(self, stats):
        #print(f'{stats.indexed_objects}/{stats.total_objects}')
        pass

    def credentials(self, url, username_from_url, allowed_types):
        if allowed_types & pygit2.enums.CredentialType.USERNAME:
            creds = self.credentials_by_type[pygit2.enums.CredentialType.USERNAME]            
        elif allowed_types & pygit2.enums.CredentialType.SSH_KEY:
            creds = self.credentials_by_type[pygit2.enums.CredentialType.SSH_KEY]
        elif allowed_types & pygit2.enums.CredentialType.USERPASS_PLAINTEXT:
            creds = self.credentials_by_type[pygit2.enums.CredentialType.USERPASS_PLAINTEXT]            
        else:
            return None

        if not creds:
            raise ValueError("Credentials not provided for the requested type: {}".format(allowed_types))            
        return creds

