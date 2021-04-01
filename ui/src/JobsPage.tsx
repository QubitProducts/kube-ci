import React, { Component, useState, useEffect } from 'react';
import PropTypes from 'prop-types';
import { makeStyles } from '@material-ui/core/styles';
import { Container } from '@material-ui/core';
import Box from '@material-ui/core/Box';
import Collapse from '@material-ui/core/Collapse';
import IconButton from '@material-ui/core/IconButton';
import Table from '@material-ui/core/Table';
import TableBody from '@material-ui/core/TableBody';
import TableCell from '@material-ui/core/TableCell';
import TableContainer from '@material-ui/core/TableContainer';
import TableHead from '@material-ui/core/TableHead';
import TableRow from '@material-ui/core/TableRow';
import Typography from '@material-ui/core/Typography';
import Paper from '@material-ui/core/Paper';
import KeyboardArrowDownIcon from '@material-ui/icons/KeyboardArrowDown';
import KeyboardArrowUpIcon from '@material-ui/icons/KeyboardArrowUp';

import Moment from 'react-moment';

import {grpc} from "@improbable-eng/grpc-web";
import { ManagerUI } from './scarab-ui_pb_service';
import { ListJobsRequest, ListJobsResponse } from './scarab-ui_pb';
import { Job } from './scarab-common_pb';
import { ManagerUIClient } from './scarab-ui_pb_service';
import JobCreateDialog from './JobCreateDialog';

const host = "http://localhost:8081";

const useRowStyles = makeStyles({
    root: {
          '& > *': {
                  borderBottom: 'unset',
                  },
          },
});

type RowProps = {
  job: Job
};

function Row(props: RowProps){
    const [open, setOpen] = React.useState(false);
    const {job} = props
    const workers = job.getWorkersList()
    const classes = useRowStyles();

    return (
          <React.Fragment>
            <TableRow className={classes.root}>
              <TableCell>
                <IconButton aria-label="expand row" size="small" onClick={() => setOpen(!open)}>
                  {open ? <KeyboardArrowUpIcon /> : <KeyboardArrowDownIcon />}
                </IconButton>
              </TableCell>
              <TableCell component="th" scope="row">
                {job?.getProfile() ?? "unknown"}
              </TableCell>
              <TableCell component="th" scope="row">
                {job?.getVersion() ?? "unknown"}
              </TableCell>
              <TableCell component="th" scope="row">
                Sometime
              </TableCell>
              <TableCell component="th" scope="row">
                {workers?.length ?? 0}
              </TableCell>
            </TableRow>
          </React.Fragment>
        );
}

type JTProps = {};

type JTState = {
    jobs: Job[],
    error: boolean
};

function JobsTable(props: JTProps){
  const [state, setState] = useState<JTState>({
      jobs: [],
      error: false,
  })

  useEffect(() => {
    const req = new ListJobsRequest();
    const client = new ManagerUIClient(host);
    client.listJobs(req, (err, resp) => {
      if (resp != null) {
        setState({
          jobs: resp.getJobsList(),
          error: false
        })
      }
    });
  },[])

  return (
    <TableContainer component={Paper}>
      <Table aria-label="collapsible table">
        <TableHead>
          <TableRow>
            <TableCell />
            <TableCell>Name</TableCell>
            <TableCell>Version</TableCell>
            <TableCell>FirstRegistration</TableCell>
            <TableCell>Workers</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {state.jobs.map((row :Job) => {
            return (
              <Row job={row} />
            )
          })}
        </TableBody>
      </Table>
    </TableContainer>
  );
}


type Props = {};

type State = {
  dialog: boolean
};

const JobsPage: React.FC<Props> = (props: Props) => {
  return (
    <div>
      <JobsTable />
      <JobCreateDialog />
    </div>
  );
}

export default JobsPage;
