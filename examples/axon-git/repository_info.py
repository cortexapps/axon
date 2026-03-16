
import json
from cortex_axon.axon_client import AxonClient, HandlerContext
from cortex_axon.handler import cortex_handler
from git_manager import manager, formatter
import os
import pygit2
from typing import Optional
from datetime import datetime, timedelta, timezone
from dataclasses import dataclass
from git_manager.models.git import Commit
from git_manager import formatter

def _get_root_dir() -> str:
    root_dir = os.getenv("GIT_ROOT_DIR")
    if not root_dir:
        return "/tmp/cortex-axon-git"
    return root_dir

@dataclass
class RepositoryInfo:
    name: str
    basePath: Optional[str] = None
    url: Optional[str] = None
    branch: Optional[str] = None

    @staticmethod
    def from_json(json_str: str) -> 'RepositoryInfo':
        json_data = json.loads(json_str)
        info = RepositoryInfo(
            name=json_data.get("name"),
            basePath=json_data.get("basePath", None),
            url=json_data.get("url", None),
            branch=json_data.get("branch", None)
        )
        return info
        

    @staticmethod
    def repo_from_context(context: HandlerContext, manager: manager.GitRepositoryManager) -> manager.GitRepository:
        info = RepositoryInfo.from_context(context)
        return manager.get(
            repo_name=info.name,
            branch=info.branch
        )

    @staticmethod
    def from_context(context: HandlerContext) -> manager.GitRepositoryManager:
        body = context.args["body"]
        if not body:
            raise ValueError("No body provided")
        
        return RepositoryInfo.from_json(body)

