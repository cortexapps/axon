import jinja2


GIT_REPO_URL_TEMPLATE_KEY = "git_repo_url"
GIT_COMMIT_URL_TEMPLATE_KEY = "git_commit_url"
GIT_BLOB_URL_TEMPLATE_KEY = "git_blob_url"
GIT_BRANCH_URL_TEMPLATE_KEY = "git_branch_url"

default_templates = {
    GIT_REPO_URL_TEMPLATE_KEY: "https://{{ git_host }}/{{ repo_name }}",
    GIT_COMMIT_URL_TEMPLATE_KEY: "https://{{ git_host }}/{{ repo_name }}/commit/{{ sha }}",
    GIT_BLOB_URL_TEMPLATE_KEY: "https://{{ git_host }}/{{ repo_name }}/blob/{{ sha }}/{{ path }}",
    GIT_BRANCH_URL_TEMPLATE_KEY: "https://{{ git_host }}/{{ repo_name }}/tree/{{ ref }}"
}

class GitFormatter:
    _global_formatter = None
    @staticmethod
    def set_global_formatter(formatter: 'GitFormtter'):
        GitFormatter._global_formatter = formatter

    @staticmethod
    def get_global_formatter() -> 'GitFormtter':
        if GitFormtter._global_formatter is None:
            raise ValueError("Global formatter not set")
        return GitFormtter._global_formatter

    def __init__(self, git_host: str, templates: dict):
        self.git_host = git_host
        self.templates = default_templates.copy()
        self.templates.update(templates)

    def __render(self, template_key: str, data: dict) -> str:
        template = self.templates.get(template_key)
        if not template:
            raise ValueError(f"Template {template_key} not found")
        return jinja2.Template(template).render(data)

    def repo_url(self, repo_name: str) -> str:
        data = {
            "git_host": self.git_host,
            "repo_name": repo_name
        }
        return self.__render(GIT_REPO_URL_TEMPLATE_KEY, data)

    def commit_url(self, repo_name: str, sha: str) -> str:
        data = {
            "git_host": self.git_host,
            "repo_name": repo_name,
            "sha": sha
        }
        return self.__render(GIT_COMMIT_URL_TEMPLATE_KEY, data)

    def blob_url(self, repo_name: str, sha: str, path: str) -> str:
        data = {
            "git_host": self.git_host,
            "repo_name": repo_name,
            "sha": sha,
            "path": path
        }
        return self.__render(GIT_BLOB_URL_TEMPLATE_KEY, data)

    def branch_url(self, repo_name: str, ref: str) -> str:
        data = {
            "git_host": self.git_host,
            "repo_name": repo_name,
            "ref": ref
        }
        return self.__render(GIT_BRANCH_URL_TEMPLATE_KEY, data)
