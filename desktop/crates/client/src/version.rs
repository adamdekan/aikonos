//! Comparing this build with the release a server advertises.

use std::cmp::Ordering;

use crate::server::ReleaseInfo;

/// This client's version.
pub const CURRENT: &str = env!("CARGO_PKG_VERSION");

/// `major.minor.patch` with an optional `-pre` part, ordered by SemVer
/// precedence (a pre-release sorts before its release). Build metadata
/// after `+` and a leading `v` are ignored.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Version {
    core: [u64; 3],
    pre: Option<String>,
}

impl Version {
    pub fn parse(text: &str) -> Option<Self> {
        let text = text.trim().trim_start_matches('v');
        let text = text.split('+').next()?;
        let (core, pre) = match text.split_once('-') {
            Some((core, pre)) => (core, Some(pre.to_owned())),
            None => (text, None),
        };
        let mut parts = core.split('.');
        let mut numbers = [0u64; 3];
        for slot in &mut numbers {
            *slot = parts.next()?.parse().ok()?;
        }
        if parts.next().is_some() {
            return None;
        }
        Some(Self { core: numbers, pre })
    }
}

impl PartialOrd for Version {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for Version {
    fn cmp(&self, other: &Self) -> Ordering {
        self.core.cmp(&other.core).then_with(|| match (&self.pre, &other.pre) {
            (None, None) => Ordering::Equal,
            (None, Some(_)) => Ordering::Greater,
            (Some(_), None) => Ordering::Less,
            (Some(a), Some(b)) => a.cmp(b),
        })
    }
}

/// What a server's advertised release means for this build.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ReleaseStatus {
    /// Nothing advertised, or this build is current.
    Current,
    /// A newer build exists; this one still works.
    UpdateAvailable {
        version: String,
        url: Option<String>,
        notes: Option<String>,
    },
    /// The server needs a newer build than this one.
    UpdateRequired {
        minimum: String,
        url: Option<String>,
        notes: Option<String>,
    },
}

pub fn release_status(release: Option<&ReleaseInfo>) -> ReleaseStatus {
    release_status_for(CURRENT, release)
}

pub fn release_status_for(current: &str, release: Option<&ReleaseInfo>) -> ReleaseStatus {
    let (Some(release), Some(current)) = (release, Version::parse(current)) else {
        return ReleaseStatus::Current;
    };
    if let Some(minimum) = release.minimum_version.as_deref()
        && let Some(required) = Version::parse(minimum)
        && current < required
    {
        return ReleaseStatus::UpdateRequired {
            minimum: minimum.to_owned(),
            url: release.url.clone(),
            notes: release.notes.clone(),
        };
    }
    match Version::parse(&release.version) {
        Some(latest) if current < latest => ReleaseStatus::UpdateAvailable {
            version: release.version.clone(),
            url: release.url.clone(),
            notes: release.notes.clone(),
        },
        _ => ReleaseStatus::Current,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn release(version: &str, minimum: Option<&str>) -> ReleaseInfo {
        ReleaseInfo {
            version: version.into(),
            minimum_version: minimum.map(Into::into),
            url: Some("https://ai.example.org/aikonos.exe".into()),
            notes: None,
        }
    }

    #[test]
    fn orders_by_semver_precedence() {
        let v = |s| Version::parse(s).unwrap();
        assert!(v("0.2.0") > v("0.1.9"));
        assert!(v("1.0.0") > v("1.0.0-rc.1"));
        assert!(v("v1.2.3+build.5") == v("1.2.3"));
        assert!(Version::parse("1.2").is_none());
        assert!(Version::parse("1.2.3.4").is_none());
    }

    #[test]
    fn minimum_beats_latest() {
        assert_eq!(
            release_status_for("0.1.0", Some(&release("0.3.0", Some("0.2.0")))),
            ReleaseStatus::UpdateRequired {
                minimum: "0.2.0".into(),
                url: Some("https://ai.example.org/aikonos.exe".into()),
                notes: None,
            }
        );
        assert!(matches!(
            release_status_for("0.2.0", Some(&release("0.3.0", Some("0.2.0")))),
            ReleaseStatus::UpdateAvailable { .. }
        ));
        assert_eq!(
            release_status_for("0.3.0", Some(&release("0.3.0", None))),
            ReleaseStatus::Current
        );
        assert_eq!(release_status_for("0.3.0", None), ReleaseStatus::Current);
        assert_eq!(
            release_status_for("0.3.0", Some(&release("not-a-version", None))),
            ReleaseStatus::Current
        );
    }
}
