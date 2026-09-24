import pygit2
from typing import Optional
from datetime import datetime, timedelta, timezone
from dataclasses import dataclass

@dataclass
class Contributor:
    email: str
    name: Optional[str] = None
    username: Optional[str] = None
    image: Optional[str] = None
    numCommits: Optional[int] = None
    url: Optional[str] = None
    alias: Optional[str] = None

    def to_dict(self) -> dict:
        return {
            "email": self.email,
            "name": self.name,
            "username": self.username,
            "image": self.image,
            "numCommits": self.numCommits,
            "url": self.url,
            "alias": self.alias
        }

@dataclass
class Commit:
    url: Optional[str]
    sha: str
    committer: Contributor
    message: str
    date: datetime

    @staticmethod
    def from_commit(commit: pygit2.Commit, url: str = None) -> 'Commit':

        username_from_email = commit.author.email and commit.author.email.split("@")[0]

        return Commit(
        url = url,
        sha = str(commit.id),
        message = commit.message.strip(),
        committer= Contributor(
            name =  commit.author.name,
            email = commit.author.email,
            username=username_from_email,
        ),
        date = _format_time(commit),
        )

    def to_dict(self) -> dict:
        return {
            "url": self.url,
            "sha": self.sha,
            "committer": self.committer.to_dict(),
            "message": "[AXON] " + self.message,
            "date": self.date,
        }


def _format_time(commit: Commit) -> str:
    ts =  datetime.fromtimestamp(commit.commit_time)
    offset = timedelta(minutes=commit.commit_time_offset)
    tz = timezone(offset)
    return ts.replace(tzinfo=tz).isoformat()