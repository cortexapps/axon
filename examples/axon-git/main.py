import json
from cortex_axon.axon_client import AxonClient, HandlerContext
from cortex_axon.handler import cortex_handler
from git_manager import manager, formatter
import os
import pygit2
from typing import Optional
from datetime import datetime, timedelta, timezone
import isodate
from dataclasses import dataclass
from git_manager.models.git import Commit
from repository_info import RepositoryInfo
from pathlib import Path
from git_manager.access import TaskManager
from git_manager.manager import GitRepositoryManager
from concurrent.futures import ThreadPoolExecutor


root_dir = "/tmp/cortex-axon-git"

formatter = formatter.GitFormatter(
    git_host = "github.com",
    templates = {
        "git_repo_url": "https://{{ git_host }}/{{ repo_name }}.git",
        "git_commit_url": "https://{{ git_host }}/{{ repo_name }}/commit/{{ sha }}",
        "git_file_url": "https://{{ git_host }}/{{ repo_name }}/blob/{{ sha }}/{{ path }}",
        "git_branch_url": "https://{{ git_host }}/{{ repo_name }}/tree/{{ ref }}",
    }
)
formatter.set_global_formatter(formatter)

repo_manager = GitRepositoryManager(root_dir=root_dir, formatter=formatter)

task_manager = TaskManager(parallel_limit=20)

def __get_body_arg(context: HandlerContext, arg: str, default: object = None) -> object:
    body = context.args.get("body")
    if not body:
        raise ValueError("No body provided")
    body_dict = json.loads(body)
    return body_dict.get(arg, default)

@cortex_handler()
def get_repo_exists(context: HandlerContext) -> str:
    
    repo = RepositoryInfo.repo_from_context(context, repo_manager)
    result = repo.exists()
    return json_dumps( {
        "exists": result
    })

@cortex_handler()
def get_repository_details(context: HandlerContext) -> str:
    
    # data class GitRepoDetails(
    #     val name: String,
    #     val url: String,
    #     val commits: List<Commit>,
    #     val contributors: List<Contributor>,
    #     val releases: List<Release>?,
    #     val language: String?,
    #     val provider: GitProvider,
    #     val missingPermissions: List<GitPermission>,
    #     val basePath: String?,
    #     val defaultBranch: String,
    #     val alias: String?,
    # )

    repo = RepositoryInfo.repo_from_context(context, repo_manager)

    commits = _get_commits(context, limit=1000, lookback="P1M")
    contributor_summary = {}
    for commit in commits:
        author = commit.committer
        if not author:
            continue
        if author.email not in contributor_summary:
            contributor_summary[author.email] = []
        contributor_summary[author.email].append(commit)

    contributors = []
    for email, user_commits in contributor_summary.items():
        c = user_commits[0].committer
        contributors.append({
            "email": c.email,
            "username": c.username or c.email,
            "name": c.name,
            "numCommits": len(user_commits),
            # "url": formatter.user_url(c.username or c.email),
            "alias": c.alias,
        })
    
    contributors.sort(key=lambda x: x["numCommits"], reverse=True)

    return json_dumps( {
        "name": repo.repo_name,
        "url": repo.repo_url,
        "commits": commits[:100],
        "contributors": contributors,
        "defaultBranch": repo.get_default_branch(),
    })


def _get_commits(context: HandlerContext, limit: int = 100, lookback: str = None) -> [Commit]:
    
    repo = RepositoryInfo.repo_from_context(context, repo_manager) 
    lookback = __get_body_arg(context, "lookback", lookback or "P7D")

    commits = task_manager.run_task(repo=repo, action = lambda: repo.commits(limit=limit))

    delta = isodate.parse_duration(lookback)
    now = datetime.now(timezone.utc)
    start_time = now - delta
    
    # Map the commits to a new shape
    mapped_commits = []
    for commit in commits:
        if commit.commit_time < start_time.timestamp():
            continue

        url = formatter.commit_url(repo.repo_name, commit.id)
        mapped_commit = Commit.from_commit(commit, url=url)
        mapped_commits.append(mapped_commit)
    return list(mapped_commits)

@cortex_handler()
def get_commits(context: HandlerContext) -> str:
    
    
    commits = _get_commits(context)

    return json_dumps({
        "commits": commits,
    })

@cortex_handler()
def get_last_commit(context: HandlerContext) -> str:
    
    repo = RepositoryInfo.repo_from_context(context, repo_manager)
    
    commits = task_manager.run_task(repo=repo, action = lambda: repo.commits(limit=1))
    # Map the commits to a new shape
    mapped_commits = []
    for commit in commits:
        url = formatter.commit_url(repo.repo_name, commit.id)
        mapped_commit = Commit.from_commit(commit, url=url)
        mapped_commits.append(mapped_commit)

    if not mapped_commits:
        return "",

    commit = mapped_commits[0] if len(mapped_commits) > 0 else None
    return json_dumps({
        "commit": commit
    })


@cortex_handler()
def get_branches(context: HandlerContext) -> str:
    
    
    repo = RepositoryInfo.repo_from_context(context, repo_manager)
    
    branches = task_manager.run_task(repo=repo, action = lambda: repo.branches())
   
    # Map the branches to a new shape
    mapped_branches = []
    for branch in branches:
        mapped_branch = {
            "name": branch.name,
            "head": str(branch.head),
            "url": formatter.branch_url(repo.repo_name, branch.name),
        }
        mapped_branches.append(mapped_branch)

    return json_dumps({
        "branches": mapped_branches,
    })

@cortex_handler()
def file_path_exists(context: HandlerContext) -> str:
    
    filePath = __get_body_arg(context, "filePath", None)
    if not filePath:
        raise ValueError("No filePath provided")

    basePath = __get_body_arg(context, "basePath", None)
    if basePath:
        filePath = os.path.join(basePath, filePath)

    repo = RepositoryInfo.repo_from_context(context, repo_manager)
    result = task_manager.run_task(repo, lambda: repo.file_path_exists(filePath))
    return json_dumps( {
        "filePath": filePath,
        "exists": result
    })


# Note: language is not supported
# data class GitSearchParams(
#     val query: String? = null,
#     val inFile: Boolean? = null,
#     val inPath: Boolean? = null,
#     val path: String? = null,
#     val fileName: String? = null,
#     val fileExtension: String? = null,
# )

def _searchfile(file: Path, query: str) -> bool:
    try:
        with open(file, "r") as f:
            content = f.read()
            return query.casefold() in content.casefold()
    except Exception as e:
        print(f"Error reading file {file}: {e}")
        return False

@cortex_handler()
def search_code(context: HandlerContext) -> str:
    
    params = __get_body_arg(context, "params", None)
    if not params:
        raise ValueError("No params provided")

    repo = RepositoryInfo.repo_from_context(context, repo_manager)    

    repo_info = RepositoryInfo.from_context(context)


    target_dir = repo.target_dir

    basePath = __get_body_arg(context, "basePath", None)
    if basePath:
        target_dir = os.path.join(target_dir, basePath)

    subPath = params.get("path", None)
    if subPath:
        target_dir = os.path.join(target_dir, subPath)

    # gather all of the files that match the search
    glob_suffix = "*"

    if params.get("fileName"):
        glob_suffix = params.get("fileName")
    elif params.get("fileExtension"):
        glob_suffix = f"*{params.get("fileExtension")}"

    files_in_scope = list(Path(target_dir).glob("**/{}".format(glob_suffix)))
    files_in_scope = [f for f in files_in_scope if f.is_file() and not str(f).startswith(".git")]
    query = params.get("query", "")

 
        
    # Trim off the target_dir from the file path
    def get_results():
        output = []
        
        if params.get("inPath"):
            files_selected = [f for f in files_in_scope if query.casefold() in str(f).casefold()]
        else:
            with ThreadPoolExecutor(max_workers=10) as executor:
                files_selected = list(executor.map(lambda f: (f, _searchfile(f, query)), files_in_scope))

        for file, found in files_selected:
            if not found:
                continue
            file_path = str(file)
            if file_path.startswith(target_dir):
                file_path = file_path[len(target_dir):]
            file_path = file_path.lstrip("/")
            output.append({
                "name": file.name,
                "path": file_path,
                "sha": repo.get_file_sha(str(file)),
                "url": formatter.blob_url(repo.repo_name, repo.sha(), file_path),
            })
        return output

    result_files = task_manager.run_task(repo, action=get_results)
    
    return json_dumps( {
        "results": result_files or [],        
    })

 

 
@cortex_handler()
def read_file(context: HandlerContext) -> str:
    
    filePath = __get_body_arg(context, "filePath", None)
    if not filePath:
        raise ValueError("No filePath provided")

    basePath = __get_body_arg(context, "basePath", None)
    if basePath:
        filePath = os.path.join(basePath, filePath)


    repo = RepositoryInfo.repo_from_context(context, repo_manager)

    result = repo.get_file_contents_string(filePath)
    return json_dumps( {
        "content": result,
    })



@cortex_handler()
def read_file_binary(context: HandlerContext) -> str:
    
    filePath = __get_body_arg(context, "filePath", None)
    if not filePath:
        raise ValueError("No filePath provided")

    basePath = __get_body_arg(context, "basePath", None)
    if basePath:
        filePath = os.path.join(basePath, filePath)


    repo = RepositoryInfo.repo_from_context(context, repo_manager)
    
    repo_info = RepositoryInfo.from_context(context)

    result = repo.get_file_contents_binary(filePath)
    return json_dumps( {
        "content": result,
    })
  

def json_dumps(data) -> str:
    return json.dumps(data, indent=4, default=lambda x: x.to_dict())

def run():
    
    repo_manager.add_credentials(
        pygit2.credentials.UserPass(
            os.getenv("GITHUB_USERNAME"),
            os.getenv("GITHUB_PASSWORD"),
        )
    )
    task_manager.run()
    client = AxonClient(scope=globals())
    client.run()

if __name__ == '__main__':
    run()


