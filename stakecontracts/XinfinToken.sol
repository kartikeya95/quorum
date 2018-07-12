pragma solidity ^0.4.21;

import "./StandardToken.sol";
import "./SafeMath.sol";
import "./BasicToken.sol";

contract XinfinToken is StandardToken {
    using SafeMath for *;
    
    mapping (address => uint256) stakedBalances;
    
    event StakeAdded(address _masternodeOwner, uint _stakeAmount);
    event StakeReleased(address _masternodeOwner, uint _stakeAmount);
    
    modifier onlyOwner() {
        require(msg.sender == owner);
        _;
    }
    
    function XinfinToken() {
        balances[msg.sender] = 1000000000000000000000000;
    }
    
    function addStake(uint256 _value, address _addr) public {
        require(balances[_addr]>= _value);
        balances[_addr] = balances[_addr].sub(_value);
        stakedBalances[_addr] = stakedBalances[_addr].add(_value);
        emit StakeAdded(_addr, _value);
    }

    function releaseStake(uint256 _value, address _addr) onlyOwner public {
        require(stakedBalances[_addr]>= _value);
        stakedBalances[_addr] = stakedBalances[_addr].sub(_value);
        balances[_addr] = balances[_addr].add(_value);
        emit StakeReleased(_addr, _value);
    }
    
}