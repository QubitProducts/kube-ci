import React from 'react';
import ProfilesTable from './ProfilesTable';
import JobsPage from './JobsPage';

const Jobs: React.FC = () => {
    return (
          <div>
          <h1>Jobs</h1>
          <JobsPage />
          </div>
        );
};

const Job: React.FC = () => {
    return (
          <div>
          <h1>Job</h1>
          </div>
        );
};

const Profiles: React.FC = () => {
    return (
          <div>
            <h1>Profiles</h1>
            <ProfilesTable />
          </div>
        );
};

const Archive: React.FC = () => {
    return (
          <h1>Archive</h1>
        );
};

const Routes = [
    {
      path: '/',
      sidebarName: 'Jobs',
      component: Jobs
    },
    {
      path: '/job/:profile/:version/:id',
      component: Job
    },
    {
      path: '/profiles',
      sidebarName: 'Profiles',
      component: Profiles
    },
    {
      path: '/archive',
      sidebarName: 'Archive',
      component: Archive
    },
];

export default Routes;
